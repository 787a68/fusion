package main

import (
	"crypto/tls"
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

var version string

// NodeObject 表示解析后的单个节点数据
type NodeObject struct {
	line   string
	prefix string
}

// Subscription 表示机场订阅项（环境变量中值以 "https://" 开头）
type Subscription struct {
	airportName string
	url         string
}

var (
	token string // 必须通过环境变量 TOKEN 设置

	// 参数映射：查询参数简写 -> Surge 完整参数
	paramMap = map[string]string{
		"udp":  "udp-relay",
		"tfo":  "tfo",
		"quic": "block-quic",
	}

	// 收集机场订阅（值以 "https://" 开头）和自建节点配置（其它文本）
	subscriptions   []Subscription
	selfNodeConfigs []string

	// 缓存全局更新结果，使用固定 key "global"
	cacheMutex     sync.Mutex
	cachedResponse = make(map[string]cachedItem)
	// 缓存持续时间：5 分钟
	cacheDuration = 5 * time.Minute

	// singleflight 用于防止多个更新同时发起
	sfGroup singleflight.Group
)

type cachedItem struct {
	timestamp time.Time
	content   string
}

// initEnv 从环境变量读取 TOKEN，并将其它环境变量按值是否以 "https://" 开头分别归类
func initEnv() {
	token = os.Getenv("TOKEN")
	if token == "" {
		log.Fatal("TOKEN environment variable is not set")
	}
	envVars := os.Environ()
	for _, envVar := range envVars {
		parts := strings.SplitN(envVar, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, value := parts[0], parts[1]
		if key == "TOKEN" {
			continue
		}
		if strings.HasPrefix(value, "https://") {
			subscriptions = append(subscriptions, Subscription{
				airportName: key,
				url:         value,
			})
		} else {
			selfNodeConfigs = append(selfNodeConfigs, value)
		}
	}
}

// removeInlineComment 去除行中以 "//" 或 "#" 开始的内联注释
func removeInlineComment(line string) string {
	re := regexp.MustCompile(`\s*(//|#).*`)
	return strings.TrimSpace(re.ReplaceAllString(line, ""))
}

// removeAllInlineComments 对文本每行去除内联注释并清除空行
func removeAllInlineComments(text string) string {
	lines := strings.Split(text, "\n")
	var cleaned []string
	for _, line := range lines {
		cleanLine := removeInlineComment(line)
		if strings.TrimSpace(cleanLine) != "" {
			cleaned = append(cleaned, cleanLine)
		}
	}
	return strings.Join(cleaned, "\n")
}

// extractProxyEntries 从文本中提取 [Proxy] 区块内的节点行
func extractProxyEntries(text string) []string {
	lines := strings.Split(text, "\n")
	var proxyEntries []string
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
			if trimmed != "" {
				proxyEntries = append(proxyEntries, trimmed)
			}
		}
	}
	return proxyEntries
}

// modifyProxyEntry 修改节点条目：更新或追加查询参数中的配置，并添加前缀（若不存在）
func modifyProxyEntry(line string, prefix string, params map[string]string) string {
	idx := strings.Index(line, "=")
	if idx == -1 {
		return line
	}
	namePart := strings.TrimSpace(line[:idx])
	configPart := line[idx:]
	if prefix != "" && !strings.HasPrefix(namePart, prefix) {
		namePart = prefix + " " + namePart
	}
	for param, newValue := range params {
		pattern := fmt.Sprintf(`\b%s=([^,\\s]+)`, param)
		re := regexp.MustCompile("(?i)" + pattern)
		if re.MatchString(configPart) {
			configPart = re.ReplaceAllString(configPart, fmt.Sprintf("%s=%s", param, newValue))
		} else {
			configPart = configPart + fmt.Sprintf(",%s=%s", param, newValue)
		}
	}
	configPart = strings.ReplaceAll(configPart, ", ", ",")
	return namePart + " " + configPart
}

// processQueryParams 根据请求中的查询参数构造配置参数映射
func processQueryParams(query map[string][]string) map[string]string {
	result := make(map[string]string)
	for short, full := range paramMap {
		if values, ok := query[short]; ok && len(values) > 0 {
			result[full] = values[0]
		}
	}
	return result
}

// updateContent 执行上游更新，并生成最终的配置内容
func updateContent(r *http.Request) (string, error) {
	var allNodeObjects []NodeObject

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{}, // 使用默认 TLS 验证
		},
	}

	var wg sync.WaitGroup
	var mu sync.Mutex

	// 并发请求所有机场订阅
	for _, sub := range subscriptions {
		wg.Add(1)
		go func(s Subscription) {
			defer wg.Done()
			log.Printf("开始请求机场订阅：%s (标识: %s)", s.url, s.airportName)
			req, err := http.NewRequest("GET", s.url, nil)
			if err != nil {
				log.Printf("构造请求失败 [%s]: %v", s.airportName, err)
				return
			}
			if ua := r.Header.Get("user-agent"); ua != "" {
				req.Header.Set("user-agent", ua)
			}
			if xsf := r.Header.Get("x-surge-unlocked-features"); xsf != "" {
				req.Header.Set("x-surge-unlocked-features", xsf)
			}
			log.Printf("上游请求 header for [%s]: %v", s.airportName, req.Header)
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("请求机场订阅失败 [%s]: %v", s.airportName, err)
				return
			}
			bodyBytes, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("读取订阅响应失败 [%s]: %v", s.airportName, err)
				return
			}
			body := string(bodyBytes)
			log.Printf("上游返回 header for [%s]: %v, 内容前200字符: %.200s", s.airportName, resp.Header, body)
			if !strings.Contains(strings.ToLower(body), "[proxy]") {
				log.Printf("跳过 [%s]：非 Surge 格式(缺少 [Proxy] 区块)", s.airportName)
				return
			}
			rawEntries := extractProxyEntries(body)
			var cleanedEntries []string
			for _, entry := range rawEntries {
				cleanedEntries = append(cleanedEntries, removeInlineComment(entry))
			}
			mu.Lock()
			for _, entry := range cleanedEntries {
				allNodeObjects = append(allNodeObjects, NodeObject{
					line:   entry,
					prefix: s.airportName,
				})
			}
			mu.Unlock()
		}(sub)
	}
	wg.Wait()

	// 处理自建节点配置
	for _, configText := range selfNodeConfigs {
		cleaned := removeAllInlineComments(configText)
		lines := strings.Split(cleaned, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				allNodeObjects = append(allNodeObjects, NodeObject{
					line:   line,
					prefix: "",
				})
			}
		}
	}

	// 过滤节点：剔除包含 "direct" 或 "reject" 的假节点，
	// 并根据等号右侧分组，重复时保留节点名称较短的记录
	nodeMap := make(map[string]NodeObject)
	for _, obj := range allNodeObjects {
		lowerLine := strings.ToLower(obj.line)
		if strings.Contains(lowerLine, "direct") || strings.Contains(lowerLine, "reject") {
			continue
		}
		idx := strings.Index(obj.line, "=")
		if idx == -1 {
			continue
		}
		name := strings.TrimSpace(obj.line[:idx])
		detail := strings.TrimSpace(obj.line[idx+1:])
		if existing, ok := nodeMap[detail]; ok {
			existingName := strings.TrimSpace(existing.line[:strings.Index(existing.line, "=")])
			if len(name) < len(existingName) {
				nodeMap[detail] = obj
			}
		} else {
			nodeMap[detail] = obj
		}
	}
	var filteredNodes []NodeObject
	for _, obj := range nodeMap {
		filteredNodes = append(filteredNodes, obj)
	}

	// 应用查询参数修改
	qParams := processQueryParams(r.URL.Query())
	var finalNodes []string
	for _, obj := range filteredNodes {
		modified := modifyProxyEntry(obj.line, obj.prefix, qParams)
		finalNodes = append(finalNodes, modified)
	}

	mergedConfig := "[Proxy]\n" + strings.Join(finalNodes, "\n")
	return mergedConfig, nil
}

// handler 负责处理 HTTP 请求，利用缓存和 singleflight 保证重复请求不重新调用上游
func handler(w http.ResponseWriter, r *http.Request) {
	// 处理 CORS 预检
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, user-agent, x-surge-unlocked-features")
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	log.Printf("客户端请求 header: %v", r.Header)
	if r.URL.Path != "/"+token {
		http.Error(w, "Forbidden: Invalid Access Path", http.StatusForbidden)
		return
	}

	// 固定缓存 key "global"
	cacheKey := "global"
	cacheMutex.Lock()
	if item, exists := cachedResponse[cacheKey]; exists && time.Since(item.timestamp) < cacheDuration {
		cacheMutex.Unlock()
		log.Printf("在缓存时间内，直接返回缓存内容")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprint(w, item.content)
		return
	}
	cacheMutex.Unlock()

	// 使用 singleflight 确保只执行一次更新
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

	// 存入缓存
	cacheMutex.Lock()
	cachedResponse[cacheKey] = cachedItem{timestamp: time.Now(), content: mergedConfig}
	cacheMutex.Unlock()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	log.Printf("返回客户端 header: %v, 内容前200字符: %.200s", w.Header(), mergedConfig)
	fmt.Fprint(w, mergedConfig)
}

func main() {
	initEnv()
	http.HandleFunc("/", handler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port
	log.Printf("Server version %s is running on %s", version, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}