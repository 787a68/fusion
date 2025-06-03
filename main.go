package main

import (
    "fmt"
    "io/ioutil"
    "log"
    "net/http"
    "os"
    "regexp"
    "sort"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "golang.org/x/sync/singleflight"
)

var version string // 版本号通过 ldflags 注入，例如：-ldflags "-X main.version=your-version"

var requestCount int64 // 客户端请求计数（原子操作）

type Subscription struct {
    airportName string
    url         string
}

var (
    token string

    // paramMap：将 URL 查询参数中的简写映射为完整配置参数名称
    paramMap = map[string]string{
        "udp":  "udp-relay",
        "tfo":  "tfo",
        "quic": "block-quic",
    }

    // 订阅节点信息，从环境变量 SUBSCRIPTIONS 读取，每个条目格式为 "名称=url"，多个条目以 "||" 分隔
    subscriptions []Subscription
    // 自建节点信息，从环境变量 CUSTOM_NODE 读取，同样以 "||" 分隔，内容直接原样使用
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

// initEnv 从环境变量中初始化 TOKEN、自建节点和订阅节点信息。
// 例如：
//   SUBSCRIPTIONS=ENET=https://106.75.141.41/YT||CNA=https://yuntong.one/yt?token=87
//   CUSTOM_NODE=自建节点1配置||自建节点2配置
func initEnv() {
    token = os.Getenv("TOKEN")
    if token == "" {
        log.Fatal("错误: TOKEN 环境变量未设置")
    }

    // 解析订阅节点信息
    subs := os.Getenv("SUBSCRIPTIONS")
    if subs != "" {
        entries := strings.Split(subs, "||")
        for _, entry := range entries {
            entry = strings.TrimSpace(entry)
            if entry == "" {
                continue
            }
            parts := strings.SplitN(entry, "=", 2)
            if len(parts) < 2 {
                log.Printf("错误: 无效的订阅项: %s", entry)
                continue
            }
            name := parts[0]
            url := parts[1]
            subscriptions = append(subscriptions, Subscription{
                airportName: name,
                url:         url,
            })
        }
    }

    // 解析自建节点信息
    nodes := os.Getenv("CUSTOM_NODE")
    if nodes != "" {
        entries := strings.Split(nodes, "||")
        for _, entry := range entries {
            entry = strings.TrimSpace(entry)
            if entry != "" {
                selfNodeConfigs = append(selfNodeConfigs, entry)
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
// 1. 保留原始左侧（如果未包含名称则自动补充）；
// 2. 将 URL 查询参数覆盖到配置部分后，将该部分中的所有空格移除。
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

// updateContent 从上游订阅获取完整代理配置，上游请求超时为 6 秒。
// 处理完订阅条目后，对订阅结果先排序，再将自建节点内容插入到最前面，并返回完整内容。
func updateContent(r *http.Request) (string, error) {
    var subEntries []string
    var mu sync.Mutex
    client := &http.Client{Timeout: 6 * time.Second}
    var wg sync.WaitGroup

    for _, sub := range subscriptions {
        wg.Add(1)
        go func(s Subscription) {
            defer wg.Done()
            log.Printf("发送订阅请求，%s，请求头：%+v", s.airportName, r.Header)
            req, err := http.NewRequest("GET", s.url, nil)
            if err != nil {
                log.Printf("创建请求失败 [%s]，错误：%v", s.airportName, err)
                return
            }
            if ua := r.Header.Get("user-agent"); ua != "" {
                req.Header.Set("user-agent", ua)
            }
            if xsf := r.Header.Get("x-surge-unlocked-features"); xsf != "" {
                req.Header.Set("x-surge-unlocked-features", xsf)
            }
            log.Printf("发送请求至 [%s]，请求头：%+v", s.airportName, req.Header)
            resp, err := client.Do(req)
            if err != nil {
                log.Printf("请求失败 [%s]，错误：%v", s.airportName, err)
                return
            }
            defer resp.Body.Close()
            log.Printf("收到 [%s] 响应，响应头：%+v", s.airportName, resp.Header)
            bodyBytes, err := ioutil.ReadAll(resp.Body)
            if err != nil {
                log.Printf("读取响应失败 [%s]，错误：%v", s.airportName, err)
                return
            }
            bodyStr := string(bodyBytes)
            log.Printf("[%s] 响应预览：%s", s.airportName, func() string {
                if len(bodyStr) > 100 {
                    return bodyStr[:100]
                }
                return bodyStr
            }())
            if !strings.Contains(strings.ToLower(bodyStr), "[proxy]") {
                log.Printf("跳过 [%s]，格式非 Surge", s.airportName)
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
    // 排序订阅条目
    sort.Strings(processedSubs)
    // 将自建节点内容插入到最前面
    finalEntries := append(selfNodeConfigs, processedSubs...)
    finalConfig := "[Proxy]\n" + strings.Join(finalEntries, "\n")
    return finalConfig, nil
}

func handler(w http.ResponseWriter, r *http.Request) {
    reqNum := atomic.AddInt64(&requestCount, 1)
    log.Printf("收到客户端请求 #%d，方法：%s，URL：%s，主机：%s，请求头：%+v",
        reqNum, r.Method, r.URL.String(), r.Host, r.Header)
    if r.Method == http.MethodOptions {
        w.Header().Set("Access-Control-Allow-Origin", "*")
        w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
        w.Header().Set("Access-Control-Allow-Headers", "Content-Type, user-agent, x-surge-unlocked-features")
        w.WriteHeader(http.StatusOK)
        return
    }
    if r.Method != http.MethodGet {
        log.Printf("鉴权失败（方法不允许） #%d，请求头：%+v", reqNum, r.Header)
        http.Error(w, "方法不允许", http.StatusMethodNotAllowed)
        return
    }
    if r.URL.Path != "/"+token {
        log.Printf("鉴权失败，路径错误 #%d，方法：%s，URL：%s，主机：%s，请求头：%+v",
            reqNum, r.Method, r.URL.String(), r.Host, r.Header)
        http.Error(w, "禁止访问：路径无效", http.StatusForbidden)
        return
    }

    cacheMutex.Lock()
    if item, exists := cachedResponse["global"]; exists && time.Since(item.timestamp) < cacheDuration {
        cacheMutex.Unlock()
        log.Printf("缓存命中，直接返回缓存内容 #%d", reqNum)
        // 直接从内存中返回缓存的完整内容
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        w.Header().Set("Content-Disposition", "attachment; filename=sub")
        w.Write([]byte(item.content))
        return
    }
    cacheMutex.Unlock()

    result, err, _ := sfGroup.Do("update", func() (interface{}, error) {
        return updateContent(r)
    })
    if err != nil {
        http.Error(w, "更新内容失败", http.StatusInternalServerError)
        return
    }
    mergedConfig, ok := result.(string)
    if !ok {
        http.Error(w, "内部错误", http.StatusInternalServerError)
        return
    }
    // 更新缓存并从内存返回
    cacheMutex.Lock()
    cachedResponse["global"] = cachedItem{timestamp: time.Now(), content: mergedConfig}
    cacheMutex.Unlock()
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("Content-Disposition", "attachment; filename=sub")
    log.Printf("返回新响应 #%d，返回完整文件", reqNum)
    w.Write([]byte(mergedConfig))
}

func main() {
    initEnv()
    http.HandleFunc("/", handler)
    port := "3000"
    addr := ":" + port
    log.Printf("服务器已启动，版本：%s，监听地址：%s", version, addr)
    log.Fatal(http.ListenAndServe(addr, nil))
}
