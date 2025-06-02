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
)

// version 将通过编译参数动态注入，例如：-ldflags "-X main.version=20250602171430"
// 如果未注入，则默认"unknown"
var version = "unknown"

// NodeObject 表示解析后的单个节点数据
type NodeObject struct {
	line   string
	prefix string
}

// Subscription 表示机场订阅项，环境变量中值以 "https://" 开头的
type Subscription struct {
	airportName string
	url         string
}

var (
	token string // 必须通过环境变量 TOKEN 设置

	// 固定的参数映射：查询参数简写 -> Surge 完整参数
	paramMap = map[string]string{
		"udp":  "udp-relay",
		"tfo":  "tfo",
		"quic": "block-quic",
	}

	// 收集机场订阅（值以 "https://" 开头）和自建节点配置（其它文本）
	subscriptions   []Subscription
	selfNodeConfigs []string

	// 缓存：对相同请求（按 r.RequestURI 做 key）在短时间内返回相同响应
	cacheMutex     sync.Mutex
	cachedResponse = make(map[string]cachedItem)
	cacheDuration  = 10 * time.Minute
)

// cachedItem 表示缓存的响应项目
type cachedItem struct {
	timestamp time.Time
	content   string
}

// initEnv 从环境变量中读取 TOKEN，并遍历所有环境变量，将非 TOKEN 的变量按是否以 "https://" 开头分别归为机场订阅与自建节点配置
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

// removeInlineComment 去除一行中以 "//" 或 "#" 开始的内联注释
func removeInlineComment(line string) string {
	re := regexp.MustCompile(`\s*(//|#).*`)
	return strings.TrimSpace(re.ReplaceAllString(line, ""))
}

// removeAllInlineComments 对整段文本中的每一行去除内联注释，并去掉空行
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
			// 遇到下一区块标识时退出
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

// modifyProxyEntry 修改单个节点条目：根据传入的参数映射更新或追加参数配置；
// 如果前缀不在节点名称开头则加上前缀；最终去除逗号后多余的空格。
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

// processQueryParams 根据 HTTP 请求中的查询参数构造参数映射（将简写替换为完整参数名）
func processQueryParams(query map[string][]string) map[string]string {
	result := make(map[string]string)
	for short, full := range paramMap {
		if values, ok := query[short]; ok && len(values) > 0 {
			result[full] = values[0]
		}
	}
	return result
}

// handler 处理 HTTP 请求，验证路径、执行缓存判断、处理订阅与自建节点，然后生成最终配置，并打印详细日志
func handler(w http.ResponseWriter, r *http.Request) {
	// 处理 OPTIONS 请求用于 CORS 预检
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

	// 记录客户端请求 header
	log.Printf("客户端请求 header: %v", r.Header)

	// 检查访问路径：仅允许 "/<TOKEN>"
	if r.URL.Path != "/"+token {
		http.Error(w, "Forbidden: Invalid Access Path", http.StatusForbidden)
		return
	}

	// 使用 URL 的 RequestURI 作为缓存 key（可根据需要扩展为加上客户端IP）
	cacheKey := r.URL.RequestURI()
	cacheMutex.Lock()
	if item, exists := cachedResponse[cacheKey]; exists {
		if time.Since(item.timestamp) < cacheDuration {
			log.Printf("重复请求, 在缓存时间内，直接返回缓存内容")
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, item.content)
			cacheMutex.Unlock()
			return
		}
	}
	cacheMutex.Unlock()

	// 根据查询参数（如 ?udp=xxx&tfo=xxx&quic=xxx）构造参数映射
	qParams := processQueryParams(r.URL.Query())
	var allNodeObjects []NodeObject

	// 定义 HTTP 客户端，超时设置为 30s（使用默认 TLS 验证，因为上游证书无误）
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// 默认 TLS 验证，只要上游证书正常即可
			TLSClientConfig: &tls.Config{},
			// 如果需要强制IPv4可在DialContext中指定"tcp4"，这里只使用默认
		},
	}

	// 并发处理机场订阅（针对每个环境变量值以 "https://" 开头的请求）
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, sub := range subscriptions {
		wg.Add(1)
		go func(sub Subscription) {
			defer wg.Done()
			log.Printf("开始请求机场订阅：%s (标识: %s)", sub.url, sub.airportName)
			req, err := http.NewRequest("GET", sub.url, nil)
			if err != nil {
				log.Printf("构造请求失败 [%s]: %v", sub.airportName, err)
				return
			}
			if ua := r.Header.Get("user-agent"); ua != "" {
				req.Header.Set("user-agent", ua)
			}
			if xsf := r.Header.Get("x-surge-unlocked-features"); xsf != "" {
				req.Header.Set("x-surge-unlocked-features", xsf)
			}
			log.Printf("上游请求 header for [%s]: %v", sub.airportName, req.Header)
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("请求机场订阅失败 [%s]: %v", sub.airportName, err)
				return
			}
			bodyBytes, err := ioutil.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("读取订阅响应失败 [%s]: %v", sub.airportName, err)
				return
			}
			body := string(bodyBytes)
			log.Printf("上游返回 header for [%s]: %v, 内容前200字符: %.200s", sub.airportName, resp.Header, body)
			if !strings.Contains(strings.ToLower(body), "[proxy]") {
				log.Printf("跳过 [%s]：非 Surge 格式(缺少 [Proxy] 区块)", sub.airportName)
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
					prefix: sub.airportName,
				})
			}
			mu.Unlock()
		}(sub)
	}
	wg.Wait()

	// 处理自建节点：对每个配置文本先去除所有注释，再按行拆分、去除行内注释
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

	// 过滤节点：剔除包含 "direct" 或 "reject"（忽略大小写）的假节点；同时根据等号右侧分组，
	// 若重复时保留节点名称较短的记录
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

	// 对过滤后的每个节点应用参数修改（更新或追加查询参数中的配置）
	var finalNodes []string
	for _, obj := range filteredNodes {
		modified := modifyProxyEntry(obj.line, obj.prefix, qParams)
		finalNodes = append(finalNodes, modified)
	}

	mergedConfig := "[Proxy]\n" + strings.Join(finalNodes, "\n")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	log.Printf("返回客户端 header: %v, 内容前200字符: %.200s", w.Header(), mergedConfig)
	// 将生成的响应存入缓存，便于后续短时间内重复请求直接返回
	cacheMutex.Lock()
	cachedResponse[cacheKey] = cachedItem{timestamp: time.Now(), content: mergedConfig}
	cacheMutex.Unlock()
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