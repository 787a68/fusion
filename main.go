package main

import (
    "crypto/rand"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "log"
    "net"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "regexp"
    "sort"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/miekg/dns"
    "golang.org/x/sync/singleflight"
)

// 全局变量
var (
    // 基础配置
    dataDir      string
    token        string
    requestCount int64
    logFile      *os.File
    logMutex     sync.Mutex

    // 缓存相关
    cacheMutex     sync.Mutex
    cachedResponse = make(map[string]cachedItem)
    cacheDuration  = 24 * time.Hour
    sfGroup        singleflight.Group

    // 节点配置
    subscriptions []Subscription

    // 参数映射
    paramMap = map[string]string{
        "udp":  "udp-relay",
        "tfo":  "tfo",
        "quic": "block-quic",
    }

    // DNS 服务器列表
    dnsServers = []string{
        "8.8.8.8",
        "1.1.1.1",
        "114.114.114.114",
    }

    // 固定 User-Agent
    surgeUA = "Surge/4.0"

    // DNS 客户端配置
    dnsClient = &dns.Client{
        Net:     "udp",
        Timeout: 5 * time.Second,
    }

    // 地理位置 API 配置
    geoAPIURL = "http://ip-api.com/json/"
    geoClient = &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            Proxy: nil, // 禁用系统代理
        },
    }

    // 国家代码到国旗 emoji 的映射
    countryEmoji = map[string]string{
        "US": "🇺🇸", "JP": "🇯🇵", "SG": "🇸🇬", "HK": "🇭🇰", "TW": "🇹🇼",
        "KR": "🇰🇷", "GB": "🇬🇧", "DE": "🇩🇪", "FR": "🇫🇷", "IT": "🇮🇹",
        "NL": "🇳🇱", "CA": "🇨🇦", "AU": "🇦🇺", "NZ": "🇳🇿", "RU": "🇷🇺",
        "BR": "🇧🇷", "IN": "🇮🇳", "ID": "🇮🇩", "MY": "🇲🇾", "TH": "🇹🇭",
        "VN": "🇻🇳", "PH": "🇵🇭", "AE": "🇦🇪", "SA": "🇸🇦", "TR": "🇹🇷",
    }

    // 默认国家代码和 emoji
    defaultCountry = "n"
    defaultEmoji   = "🇺🇸"
)

// 数据结构定义
type (
    // 订阅配置
    Subscription struct {
        airportName string
        url         string
    }

    // 节点信息
    Node struct {
        fullName    string
        protocol    string
        server      string
        port        string
        params      map[string]string
        country     string
        emoji       string
        entryPoint  string // 入口点（域名或IP）
        isDomain    bool   // 是否是域名
    }

    // 缓存项
    cachedItem struct {
        timestamp time.Time
        content   string
    }

    // 日志条目
    LogEntry struct {
        Timestamp string `json:"timestamp"`
        Level     string `json:"level"`
        Message   string `json:"message"`
        RequestID int64  `json:"request_id,omitempty"`
        URL       string `json:"url,omitempty"`
        Method    string `json:"method,omitempty"`
        Status    int    `json:"status,omitempty"`
    }
)

// 初始化函数
func init() {
    log.SetFlags(0)
}

// 主函数
func main() {
    // 1. 初始化环境
    if err := initEnv(); err != nil {
        log.Fatalf("初始化环境失败: %v", err)
    }

    // 2. 启动缓存检查
    go startCacheCheck()

    // 3. 启动 HTTP 服务器
    http.HandleFunc("/fusion", handler)
    log.Printf("服务器启动成功，监听端口 8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}

// 环境初始化
func initEnv() error {
    // 1. 设置数据目录
    if err := setupDataDir(); err != nil {
        return fmt.Errorf("设置数据目录失败: %v", err)
    }

    // 2. 设置日志
    if err := setupLogging(); err != nil {
        return fmt.Errorf("设置日志失败: %v", err)
    }

    // 3. 加载缓存
    if err := loadCache(); err != nil {
        log.Printf("加载缓存失败: %v", err)
    }

    // 4. 初始化 token
    if err := initToken(); err != nil {
        return fmt.Errorf("初始化 token 失败: %v", err)
    }

    // 5. 加载配置
    if err := loadConfig(); err != nil {
        return fmt.Errorf("加载配置失败: %v", err)
    }

    return nil
}

// 设置数据目录
func setupDataDir() error {
    dataDir = os.Getenv("DATA_DIR")
    if dataDir == "" {
        dataDir = "data"
    }
    return os.MkdirAll(dataDir, 0755)
}

// 设置日志
func setupLogging() error {
    logFileName := filepath.Join(dataDir, fmt.Sprintf("fusion_%s.log", time.Now().Format("2006-01-02")))
    f, err := os.OpenFile(logFileName, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return err
    }
    logFile = f
    log.SetOutput(f)
    return nil
}

// 初始化 token
func initToken() error {
    if token = os.Getenv("TOKEN"); token == "" {
        var err error
        token, err = loadToken()
        if err != nil {
            return err
        }
        if token == "" {
            token, err = generateToken()
            if err != nil {
                return err
            }
            if err := saveToken(); err != nil {
                return err
            }
            writeLog("INFO", fmt.Sprintf("自动生成的 TOKEN: %s", token), 0, nil, 0)
        }
    }
    return nil
}

// 加载配置
func loadConfig() error {
    // 加载订阅配置
    if subs := os.Getenv("SUBSCRIPTIONS"); subs != "" {
        for _, entry := range strings.Split(subs, ",") {
            if parts := strings.SplitN(strings.TrimSpace(entry), "=", 2); len(parts) == 2 {
                subscriptions = append(subscriptions, Subscription{
                    airportName: parts[0],
                    url:         parts[1],
                })
            }
        }
    }
    return nil
}

// HTTP 处理器
func handler(w http.ResponseWriter, r *http.Request) {
    requestID := atomic.AddInt64(&requestCount, 1)
    writeLog("INFO", "收到请求", requestID, r, 0)

    // 验证 token
    if r.URL.Query().Get("t") != token {
        writeLog("ERROR", "无效的 token", requestID, r, http.StatusUnauthorized)
        http.Error(w, "", http.StatusUnauthorized)
        return
    }

    // 检查是否需要强制更新
    if r.URL.Query().Has("f") {
        // 启动异步更新
        go func() {
            if err := forceUpdate(); err != nil {
                writeLog("ERROR", fmt.Sprintf("强制更新失败: %v", err), requestID, nil, 0)
            }
        }()
        // 立即返回
        w.WriteHeader(http.StatusOK)
        return
    }

    // 处理普通请求
    content, err := processRequest(r)
    if err != nil {
        writeLog("ERROR", err.Error(), requestID, r, http.StatusInternalServerError)
        http.Error(w, "", http.StatusInternalServerError)
        return
    }

    // 返回响应
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Write([]byte(content))
    writeLog("INFO", "请求处理完成", requestID, r, http.StatusOK)
}

// 添加强制更新函数
func forceUpdate() error {
    // 获取所有节点
    nodes, err := fetchAllNodes()
    if err != nil {
        return fmt.Errorf("获取节点失败: %v", err)
    }

    // 处理节点
    content := processNodes(nodes, url.Values{})

    // 更新缓存
    updateCache("default", content)
    return nil
}

// 处理请求
func processRequest(r *http.Request) (string, error) {
    // 读取缓存
    content, err := getCachedContent("default")
    if err != nil {
        return "", err
    }

    // 应用参数
    return applyParams(content, r.URL.Query())
}

// 获取所有节点
func fetchAllNodes() ([]*Node, error) {
    var allNodes []*Node
    var mu sync.Mutex
    var wg sync.WaitGroup

    for _, sub := range subscriptions {
        wg.Add(1)
        go func(s Subscription) {
            defer wg.Done()
            nodes, err := fetchSubscription(s)
            if err != nil {
                writeLog("ERROR", fmt.Sprintf("获取订阅失败 %s: %v", s.airportName, err), 0, nil, 0)
                return
            }
            mu.Lock()
            allNodes = append(allNodes, nodes...)
            mu.Unlock()
        }(sub)
    }
    wg.Wait()

    return allNodes, nil
}

// 获取订阅内容
func fetchSubscription(sub Subscription) ([]*Node, error) {
    client := &http.Client{Timeout: 10 * time.Second}
    req, err := http.NewRequest("GET", sub.url, nil)
    if err != nil {
        return nil, err
    }
    req.Header.Set("User-Agent", surgeUA)

    resp, err := client.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }

    return parseSubscription(string(body), sub.airportName)
}

// 解析订阅内容
func parseSubscription(content, airportName string) ([]*Node, error) {
    var nodes []*Node
    inProxySection := false

    lines := strings.Split(content, "\n")
    for _, line := range lines {
        line = strings.TrimSpace(line)
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }

        if strings.ToLower(line) == "[proxy]" {
            inProxySection = true
            continue
        }

        if inProxySection && strings.HasPrefix(line, "[") {
            break
        }

        if inProxySection && strings.Contains(line, "=") {
            node := parseNode(line)
            if node != nil {
                node.fullName = fmt.Sprintf("%s %s", airportName, node.fullName)
                nodes = append(nodes, node)
            }
        }
    }

    return nodes, nil
}

// 获取节点国家信息
func getNodeCountry(node *Node) (string, string, error) {
    // 创建代理客户端
    proxyClient, err := createProxyClient(node)
    if err != nil {
        return "", "", fmt.Errorf("创建代理客户端失败: %v", err)
    }

    // 通过代理请求地理位置 API
    country, err := getCountryViaProxy(proxyClient)
    if err != nil {
        return "", "", fmt.Errorf("获取地理位置失败: %v", err)
    }

    emoji := countryEmoji[country]
    if emoji == "" {
        emoji = "🌐" // 使用通用emoji作为默认值
    }

    return country, emoji, nil
}

// 创建代理客户端
func createProxyClient(node *Node) (*http.Client, error) {
    var proxyURL string
    switch node.protocol {
    case "ss":
        proxyURL = createSSProxyURL(node)
    case "trojan":
        proxyURL = createTrojanProxyURL(node)
    case "vmess":
        proxyURL = createVmessProxyURL(node)
    default:
        return nil, fmt.Errorf("不支持的代理协议: %s", node.protocol)
    }

    proxy, err := url.Parse(proxyURL)
    if err != nil {
        return nil, err
    }

    return &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            Proxy: http.ProxyURL(proxy),
        },
    }, nil
}

// 创建 SS 代理 URL
func createSSProxyURL(node *Node) string {
    // 构建 SS 代理 URL
    // 格式: ss://method:password@host:port
    method := node.params["method"]
    password := node.params["password"]
    return fmt.Sprintf("ss://%s:%s@%s:%s", method, password, node.server, node.port)
}

// 创建 Trojan 代理 URL
func createTrojanProxyURL(node *Node) string {
    // 构建 Trojan 代理 URL
    // 格式: trojan://password@host:port?security=tls&sni=xxx
    password := node.params["password"]
    sni := node.params["sni"]
    return fmt.Sprintf("trojan://%s@%s:%s?security=tls&sni=%s", password, node.server, node.port, sni)
}

// 创建 Vmess 代理 URL
func createVmessProxyURL(node *Node) string {
    // 构建 Vmess 代理 URL
    // 格式: vmess://base64(json)
    vmessConfig := map[string]interface{}{
        "v":    "2",
        "ps":   node.fullName,
        "add":  node.server,
        "port": node.port,
        "id":   node.params["uuid"],
        "aid":  node.params["alterId"],
        "net":  node.params["network"],
        "type": "none",
        "host": node.params["host"],
        "path": node.params["path"],
        "tls":  node.params["tls"],
    }

    jsonData, err := json.Marshal(vmessConfig)
    if err != nil {
        return ""
    }

    return fmt.Sprintf("vmess://%s", base64.StdEncoding.EncodeToString(jsonData))
}

// 通过代理获取国家信息
func getCountryViaProxy(client *http.Client) (string, error) {
    resp, err := client.Get(geoAPIURL)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return "", err
    }

    var result struct {
        CountryCode string `json:"countryCode"`
    }

    if err := json.Unmarshal(body, &result); err != nil {
        return "", err
    }

    return result.CountryCode, nil
}

// 处理节点域名
func processNodeDomain(node *Node) ([]*Node, error) {
    if node == nil {
        return nil, fmt.Errorf("节点不能为空")
    }

    if !node.isDomain {
        return []*Node{node}, nil
    }

    // 获取域名的所有 IP
    ips, err := getIPsForDomain(node.server)
    if err != nil {
        return nil, fmt.Errorf("处理域名 %s 失败: %v", node.server, err)
    }

    if len(ips) == 0 {
        return nil, fmt.Errorf("域名 %s 没有可用的 IP", node.server)
    }

    if len(ips) == 1 {
        // 只有一个 IP，直接修改节点
        node.server = ips[0]
        node.isDomain = false
        return []*Node{node}, nil
    }

    // 多个 IP，分裂节点
    var nodes []*Node
    for _, ip := range ips {
        newNode := *node
        newNode.server = ip
        newNode.isDomain = false
        nodes = append(nodes, &newNode)
    }

    return nodes, nil
}

// 处理节点 SNI
func processNodeSNI(node *Node) error {
    if node == nil {
        return fmt.Errorf("节点不能为空")
    }

    // 检查是否需要 SNI
    needsSNI := node.protocol == "trojan" || 
                (node.protocol == "vmess" && node.params["tls"] == "tls") ||
                (node.protocol == "ss" && node.params["plugin"] == "v2ray-plugin")

    if !needsSNI {
        return nil
    }

    // 如果已经有 SNI，保持不变
    if _, hasSNI := node.params["sni"]; hasSNI {
        return nil
    }

    // 如果是域名，使用域名作为 SNI
    if node.isDomain {
        node.params["sni"] = node.server
        return nil
    }

    return fmt.Errorf("无法为节点设置 SNI: 不是域名且没有现有 SNI")
}

// 获取域名的所有 IP
func getIPsForDomain(domain string) ([]string, error) {
    if domain == "" {
        return nil, fmt.Errorf("域名不能为空")
    }

    var ips []string
    var mu sync.Mutex
    var wg sync.WaitGroup
    errChan := make(chan error, len(dnsServers))
    successChan := make(chan struct{})

    for _, dnsServer := range dnsServers {
        wg.Add(1)
        go func(server string) {
            defer wg.Done()
            if ip, err := queryDNS(domain, server); err == nil && ip != "" {
                mu.Lock()
                // 检查 IP 是否已存在
                exists := false
                for _, existingIP := range ips {
                    if existingIP == ip {
                        exists = true
                        break
                    }
                }
                if !exists {
                    ips = append(ips, ip)
                }
                mu.Unlock()
                successChan <- struct{}{}
            } else if err != nil {
                errChan <- err
            }
        }(dnsServer)
    }

    // 等待至少一个成功或所有失败
    go func() {
        wg.Wait()
        close(errChan)
        close(successChan)
    }()

    // 检查是否有成功
    successCount := 0
    for range successChan {
        successCount++
    }

    // 如果没有成功，返回错误
    if successCount == 0 {
        var errs []string
        for err := range errChan {
            errs = append(errs, err.Error())
        }
        return nil, fmt.Errorf("所有 DNS 查询失败: %s", strings.Join(errs, "; "))
    }

    return ips, nil
}

// 查询 DNS
func queryDNS(domain, dnsServer string) (string, error) {
    m := new(dns.Msg)
    m.SetQuestion(dns.Fqdn(domain), dns.TypeA)
    m.RecursionDesired = true

    r, _, err := dnsClient.Exchange(m, dnsServer+":53")
    if err != nil {
        return "", err
    }

    if len(r.Answer) == 0 {
        return "", nil
    }

    if a, ok := r.Answer[0].(*dns.A); ok {
        return a.A.String(), nil
    }

    return "", nil
}

// 修改 processNodes 函数
func processNodes(nodes []*Node, query url.Values) string {
    if len(nodes) == 0 {
        return ""
    }

    var allNodes []*Node
    var wg sync.WaitGroup
    var mu sync.Mutex
    errChan := make(chan error, len(nodes))

    // 并行处理所有节点
    for _, node := range nodes {
        wg.Add(1)
        go func(n *Node) {
            defer wg.Done()

            // 获取节点国家信息
            country, emoji, err := getNodeCountry(n)
            if err != nil {
                errChan <- fmt.Errorf("节点 %s 获取国家信息失败: %v", n.server, err)
                return
            }

            n.country = country
            n.emoji = emoji

            // 处理节点域名
            processedNodes, err := processNodeDomain(n)
            if err != nil {
                errChan <- fmt.Errorf("节点 %s 处理域名失败: %v", n.server, err)
                return
            }

            // 处理每个节点的 SNI
            for _, node := range processedNodes {
                if err := processNodeSNI(node); err != nil {
                    errChan <- fmt.Errorf("节点 %s 处理 SNI 失败: %v", node.server, err)
                    return
                }
            }

            mu.Lock()
            allNodes = append(allNodes, processedNodes...)
            mu.Unlock()
        }(node)
    }

    wg.Wait()
    close(errChan)

    // 检查是否有错误
    for err := range errChan {
        writeLog("ERROR", err.Error(), 0, nil, 0)
    }

    // 如果没有有效节点，返回空字符串
    if len(allNodes) == 0 {
        return ""
    }

    // 去重并重命名节点
    return deduplicateAndRenameNodes(allNodes, query)
}

// 去重并重命名节点
func deduplicateAndRenameNodes(nodes []*Node, query url.Values) string {
    if len(nodes) == 0 {
        return ""
    }

    // 创建节点映射
    nodeMap := make(map[string]*Node)
    for _, node := range nodes {
        if node == nil {
            continue
        }

        // 创建唯一键
        key := fmt.Sprintf("%s:%s:%s:%s", node.protocol, node.server, node.port, node.country)
        
        // 如果节点已存在，跳过
        if _, exists := nodeMap[key]; exists {
            continue
        }
        
        // 应用参数
        applyNodeParams(node, query)
        nodeMap[key] = node
    }

    // 如果没有有效节点，返回空字符串
    if len(nodeMap) == 0 {
        return ""
    }

    // 按国家分组计数
    countryCount := make(map[string]int)
    for _, node := range nodeMap {
        countryCount[node.country]++
    }

    // 重命名节点
    var result []string
    currentCount := make(map[string]int)
    for _, node := range nodeMap {
        count := currentCount[node.country]
        currentCount[node.country]++
        node.fullName = fmt.Sprintf("%s %s-%s-%d", node.fullName, node.emoji, node.country, count+1)
        result = append(result, buildNodeString(node))
    }

    return strings.Join(result, "\n")
}

// 应用节点参数
func applyNodeParams(node *Node, query url.Values) {
    // 应用参数
    for param, value := range query {
        if fullParam, ok := paramMap[param]; ok {
            node.params[fullParam] = value[0]
        }
    }

    // 处理布尔值参数
    for k, v := range node.params {
        if v == "true" {
            node.params[k] = "1"
        } else if v == "false" {
            node.params[k] = "0"
        }
    }
}

// 启动缓存检查
func startCacheCheck() {
    ticker := time.NewTicker(2 * time.Hour)
    for range ticker.C {
        if err := checkCache(); err != nil {
            writeLog("ERROR", fmt.Sprintf("缓存检查失败: %v", err), 0, nil, 0)
        }
    }
}

// 检查缓存
func checkCache() error {
    cacheMutex.Lock()
    defer cacheMutex.Unlock()

    if item, ok := cachedResponse["default"]; ok {
        if time.Since(item.timestamp) > cacheDuration {
            // 执行更新
            nodes, err := fetchAllNodes()
            if err != nil {
                return err
            }

            content := processNodes(nodes, url.Values{})
            updateCache("default", content)
        }
    }
    return nil
}

// 应用参数
func applyParams(content string, query url.Values) (string, error) {
    var result []string
    lines := strings.Split(content, "\n")

    for _, line := range lines {
        node := parseNode(line)
        if node == nil {
            continue
        }

        // 应用参数
        for param, value := range query {
            if fullParam, ok := paramMap[param]; ok {
                node.params[fullParam] = value[0]
            }
        }

        // 处理布尔值参数
        for k, v := range node.params {
            if v == "true" {
                node.params[k] = "1"
            } else if v == "false" {
                node.params[k] = "0"
            }
        }

        result = append(result, buildNodeString(node))
    }

    return strings.Join(result, "\n"), nil
}

// 其他辅助函数
func generateToken() (string, error) {
    b := make([]byte, 32)
    if _, err := rand.Read(b); err != nil {
        return "", err
    }
    return base64.URLEncoding.EncodeToString(b), nil
}

func writeLog(level, message string, requestID int64, r *http.Request, status int) {
    entry := LogEntry{
        Timestamp: time.Now().Format("2006-01-02 15:04:05"),
        Level:     level,
        Message:   message,
        RequestID: requestID,
    }

    if r != nil {
        entry.URL = r.URL.String()
        entry.Method = r.Method
        entry.Status = status
    }

    logMutex.Lock()
    defer logMutex.Unlock()

    if logFile != nil {
        log.Printf("%s [%s] %s", entry.Timestamp, entry.Level, entry.Message)
    }
}

// 缓存相关函数
func getCachedContent(key string) (string, bool) {
    cacheMutex.Lock()
    defer cacheMutex.Unlock()

    if item, ok := cachedResponse[key]; ok {
        if time.Since(item.timestamp) < cacheDuration {
            return item.content, true
        }
    }
    return "", false
}

func updateCache(key string, content string) {
    cacheMutex.Lock()
    cachedResponse[key] = cachedItem{
        timestamp: time.Now(),
        content:   content,
    }
    cacheMutex.Unlock()

    go saveCache()
}

func saveCache() error {
    cacheFile := filepath.Join(dataDir, "cache.json")
    cacheMutex.Lock()
    defer cacheMutex.Unlock()

    data := make(map[string]cachedItem)
    for k, v := range cachedResponse {
        data[k] = v
    }

    jsonData, err := json.MarshalIndent(data, "", "  ")
    if err != nil {
        return err
    }

    return ioutil.WriteFile(cacheFile, jsonData, 0644)
}

func loadCache() error {
    cacheFile := filepath.Join(dataDir, "cache.json")
    if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
        return nil
    }

    data, err := ioutil.ReadFile(cacheFile)
    if err != nil {
        return err
    }

    var cacheData map[string]cachedItem
    if err := json.Unmarshal(data, &cacheData); err != nil {
        return err
    }

    cacheMutex.Lock()
    cachedResponse = cacheData
    cacheMutex.Unlock()

    return nil
}

// Token 相关函数
func saveToken() error {
    return ioutil.WriteFile(filepath.Join(dataDir, "token.txt"), []byte(token), 0600)
}

func loadToken() (string, error) {
    tokenFile := filepath.Join(dataDir, "token.txt")
    if _, err := os.Stat(tokenFile); os.IsNotExist(err) {
        return "", nil
    }

    data, err := ioutil.ReadFile(tokenFile)
    if err != nil {
        return "", err
    }

    return strings.TrimSpace(string(data)), nil
}

// 其他辅助函数
func generateCacheKey(params map[string]string) string {
    var keys []string
    for k := range params {
        keys = append(keys, k)
    }
    sort.Strings(keys)

    var parts []string
    for _, k := range keys {
        parts = append(parts, fmt.Sprintf("%s=%s", k, params[k]))
    }
    return strings.Join(parts, "&")
}

func processQueryParams(query map[string][]string) map[string]string {
    params := make(map[string]string)
    for k, v := range query {
        if len(v) > 0 {
            params[k] = v[0]
        }
    }
    return params
}

func parseNode(line string) *Node {
    // 解析节点配置
    // 这里需要根据实际的节点格式进行解析
    // 示例实现
    parts := strings.Split(line, ",")
    if len(parts) < 3 {
        return nil
    }

    node := &Node{
        protocol: parts[0],
        server:   parts[1],
        port:     parts[2],
        params:   make(map[string]string),
    }

    if len(parts) > 3 {
        for i := 3; i < len(parts); i++ {
            if strings.Contains(parts[i], "=") {
                kv := strings.SplitN(parts[i], "=", 2)
                node.params[kv[0]] = kv[1]
            }
        }
    }

    return node
}

func buildNodeString(node *Node) string {
    var parts []string
    parts = append(parts, node.protocol, node.server, node.port)

    for k, v := range node.params {
        parts = append(parts, fmt.Sprintf("%s=%s", k, v))
    }

    return strings.Join(parts, ",")
}

