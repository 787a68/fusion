package main

import (
    "context"
    "fmt"
    "io/ioutil"
    "log"
    "net/http"
    "os"
    "regexp"
    "strings"
    "sync"
    "time"

    "golang.org/x/sync/singleflight"
)

var version string // 版本号通过 ldflags 注入，例如：-ldflags "-X main.version=your-version"

type Subscription struct {
    airportName string
    url         string
}

var (
    token string

    // paramMap 用于将 URL 查询参数中的简写映射为完整配置参数名
    paramMap = map[string]string{
        "udp":  "udp-relay",
        "tfo":  "tfo",
        "quic": "block-quic",
    }

    subscriptions   []Subscription
    selfNodeConfigs []string

    cacheMutex     sync.Mutex
    cachedResponse = make(map[string]cachedItem)
    cacheDuration  = 5 * time.Minute

    sfGroup singleflight.Group // 保证同一时间只有一次更新操作
)

type cachedItem struct {
    timestamp time.Time
    content   string
}

// initEnv 从环境变量中初始化 TOKEN，自建节点（CUSTOM_NODE）以及订阅机场。
// 固定变量为 TOKEN 和 CUSTOM_NODE，其它所有环境变量均视为订阅机场。
func initEnv() {
    token = os.Getenv("TOKEN")
    if token == "" {
        log.Fatal("TOKEN environment variable is not set")
    }

    for _, envVar := range os.Environ() {
        parts := strings.SplitN(envVar, "=", 2)
        if len(parts) != 2 {
            continue
        }
        key, value := parts[0], parts[1]
        if key == "TOKEN" {
            continue
        }
        if key == "CUSTOM_NODE" {
            // 自建节点直接使用原始内容
            if value != "" {
                selfNodeConfigs = append(selfNodeConfigs, value)
            }
        } else {
            // 其他均视为订阅机场
            if value != "" {
                subscriptions = append(subscriptions, Subscription{
                    airportName: key,
                    url:         value,
                })
            }
        }
    }
}

func cleanLine(line string) string {
    re := regexp.MustCompile(`\s*(//|#).*`)
    return strings.TrimSpace(re.ReplaceAllString(line, ""))
}

func extractProxyEntries(text string) []string {
    lines := strings.Split(text, "\n")
    var entries []string
    inProxySection := false
    for _, line := range lines {
        trimmed := strings.TrimSpace(line)
        if !inProxySection {
            if strings.ToLower(trimmed) == "[proxy]" {
                inProxySection = true
            }
        } else {
            if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
                break
            }
            if trimmed != "" && strings.Contains(trimmed, "=") {
                entries = append(entries, trimmed)
            }
        }
    }
    return entries
}

// modifyProxyEntry 对订阅条目进行格式处理：
// 保留原始左侧（如果未包含机场名称则添加），
// 将 URL 查询参数覆盖到配置部分后移除配置部分内的所有空格。
func modifyProxyEntry(line string, prefix string, params map[string]string) string {
    idx := strings.Index(line, "=")
    if idx == -1 {
        return line
    }
    namePart := line[:idx]
    configPart := line[idx+1:]
    if prefix != "" && !strings.HasPrefix(namePart, prefix) {
        namePart = prefix + " " + namePart
    }
    for param, newValue := range params {
        pattern := fmt.Sprintf(`\b%s=([^,\s]+)`, param)
        re := regexp.MustCompile("(?i)" + pattern)
        if re.MatchString(configPart) {
            configPart = re.ReplaceAllString(configPart, fmt.Sprintf("%s=%s", param, newValue))
        } else {
            configPart = configPart + fmt.Sprintf(",%s=%s", param, newValue)
        }
    }
    configPart = strings.ReplaceAll(configPart, " ", "")
    return namePart + "=" + configPart
}

func processQueryParams(query map[string][]string) map[string]string {
    result := make(map[string]string)
    for short, full := range paramMap {
        if values, ok := query[short]; ok && len(values) > 0 {
            result[full] = values[0]
        }
    }
    return result
}

func updateContent(r *http.Request) (string, error) {
    var subEntries []string
    var mu sync.Mutex
    client := &http.Client{}
    var wg sync.WaitGroup

    for _, sub := range subscriptions {
        wg.Add(1)
        go func(s Subscription) {
            defer wg.Done()
            log.Printf("Sending subscription request for: %s, Request Header: %+v", s.airportName, r.Header)
            req, err := http.NewRequest("GET", s.url, nil)
            if err != nil {
                log.Printf("Failed to create request [%s]: %v", s.airportName, err)
                return
            }
            if ua := r.Header.Get("user-agent"); ua != "" {
                req.Header.Set("user-agent", ua)
            }
            if xsf := r.Header.Get("x-surge-unlocked-features"); xsf != "" {
                req.Header.Set("x-surge-unlocked-features", xsf)
            }
            log.Printf("Outgoing request for [%s] Header: %+v", s.airportName, req.Header)
            resp, err := client.Do(req)
            if err != nil {
                log.Printf("Request failed [%s]: %v", s.airportName, err)
                return
            }
            defer resp.Body.Close()
            log.Printf("Received response for [%s] Header: %+v", s.airportName, resp.Header)
            bodyBytes, err := ioutil.ReadAll(resp.Body)
            if err != nil {
                log.Printf("Failed to read response [%s]: %v", s.airportName, err)
                return
            }
            bodyStr := string(bodyBytes)
            preview := bodyStr
            if len(preview) > 100 {
                preview = preview[:100]
            }
            log.Printf("Received response for [%s] Body Preview: %s", s.airportName, preview)
            if !strings.Contains(strings.ToLower(bodyStr), "[proxy]") {
                log.Printf("Skipping [%s]: Not Surge format", s.airportName)
                return
            }
            entries := extractProxyEntries(bodyStr)
            mu.Lock()
            for _, entry := range entries {
                if entry != "" && strings.Contains(entry, "=") {
                    parts := strings.SplitN(entry, "=", 2)
                    if !strings.HasPrefix(parts[0], s.airportName) {
                        entry = s.airportName + " " + entry
                    }
                    subEntries = append(subEntries, entry)
                }
            }
            mu.Unlock()
        }(sub)
    }
    wg.Wait()

    qParams := processQueryParams(r.URL.Query())
    var processedSubs []string
    dedupMap := make(map[string]string)
    for _, entry := range subEntries {
        modified := modifyProxyEntry(entry, "", qParams)
        lower := strings.ToLower(modified)
        if strings.Contains(lower, "direct") || strings.Contains(lower, "reject") {
            continue
        }
        parts := strings.SplitN(modified, "=", 2)
        if len(parts) != 2 {
            continue
        }
        left := parts[0]
        right := parts[1]
        if existing, found := dedupMap[right]; found {
            existingLeft := strings.SplitN(existing, "=", 2)[0]
            if len(left) < len(existingLeft) {
                dedupMap[right] = modified
            }
        } else {
            dedupMap[right] = modified
        }
    }
    for _, v := range dedupMap {
        processedSubs = append(processedSubs, v)
    }

    var selfEntries []string
    for _, config := range selfNodeConfigs {
        if config != "" {
            selfEntries = append(selfEntries, config)
        }
    }

    finalEntries := append(processedSubs, selfEntries...)
    finalConfig := "[Proxy]\n" + strings.Join(finalEntries, "\n")
    return finalConfig, nil
}

func heartbeat(ctx context.Context, w http.ResponseWriter) {
    select {
    case <-time.After(1 * time.Second):
    case <-ctx.Done():
        return
    }
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()
    timeout := time.After(30 * time.Second)
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if flusher, ok := w.(http.Flusher); ok {
                flusher.Flush()
            }
        case <-timeout:
            return
        }
    }
}

func heartbeatHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/plain")
    w.WriteHeader(http.StatusOK)
    fmt.Fprint(w, "pong")
}

func handler(w http.ResponseWriter, r *http.Request) {
    log.Printf("Received client request: Method: %s, URL: %s, Host: %s, Headers: %+v",
        r.Method, r.URL.String(), r.Host, r.Header)
    if r.Method == http.MethodOptions {
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, user-agent, x-surge-unlocked-features")
        w.WriteHeader(http.StatusOK)
        return
    }
    if r.Method != http.MethodGet {
        log.Printf("Authentication failed (method not allowed): %+v", r.Header)
        http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
        return
    }
    if r.URL.Path != "/"+token {
        log.Printf("Authentication failed: Request: Method: %s, URL: %s, Host: %s, Headers: %+v",
            r.Method, r.URL.String(), r.Host, r.Header)
        http.Error(w, "Forbidden: Invalid Access Path", http.StatusForbidden)
        return
    }

    cacheMutex.Lock()
    if item, exists := cachedResponse["global"]; exists && time.Since(item.timestamp) < cacheDuration {
        cacheMutex.Unlock()
        log.Printf("Cache hit. Returning cached response. Request Headers: %+v", r.Header)
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        log.Printf("Returning response to client: Headers: %+v, Body: %s", w.Header(), item.content)
        w.Write([]byte(item.content))
        return
    }
    cacheMutex.Unlock()

    ctx, cancel := context.WithCancel(r.Context())
    defer cancel()
    go heartbeat(ctx, w)
    result, err, _ := sfGroup.Do("update", func() (interface{}, error) {
        return updateContent(r)
    })
    if err != nil {
        http.Error(w, "Failed to update content", http.StatusInternalServerError)
        return
    }
    mergedConfig, ok := result.(string)
    if !ok {
        http.Error(w, "Internal Server Error", http.StatusInternalServerError)
        return
    }
    cacheMutex.Lock()
    cachedResponse["global"] = cachedItem{timestamp: time.Now(), content: mergedConfig}
    cacheMutex.Unlock()
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    log.Printf("Returning new response to client: Headers: %+v, Body: %s", w.Header(), mergedConfig)
    fmt.Fprint(w, mergedConfig)
}

func main() {
    initEnv()
    http.HandleFunc("/heartbeat", heartbeatHandler)
    http.HandleFunc("/", handler)

    // 固定端口为3000
    port := "3000"
    addr := ":" + port
    // 直接使用构建时通过 ldflags 注入的版本号，不判断
    log.Printf("Server started. Version: %s, Listening on %s", version, addr)
    log.Fatal(http.ListenAndServe(addr, nil))
}