package main

import (
    "encoding/base64"
    "encoding/json"
    "fmt"
    "io/ioutil"
    "log"
    "net"
    "net/http"
    "net/url"
    "strings"
    "sync"
    "time"

    "github.com/miekg/dns"
)

// 地理位置 API 配置
var (
    geoAPIURL = "http://ip-api.com/json/"
    geoClient = &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            Proxy: nil, // 禁用系统代理
        },
    }
)

// 创建代理客户端
func createProxyClient(node *Node) (*http.Client, error) {
    if node == nil {
        return nil, fmt.Errorf("节点不能为空")
    }

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
        return nil, fmt.Errorf("解析代理 URL 失败: %v", err)
    }

    return &http.Client{
        Timeout: 10 * time.Second,
        Transport: &http.Transport{
            Proxy: http.ProxyURL(proxy),
            DialContext: (&net.Dialer{
                Timeout:   30 * time.Second,
                KeepAlive: 30 * time.Second,
            }).DialContext,
            MaxIdleConns:          100,
            IdleConnTimeout:       90 * time.Second,
            TLSHandshakeTimeout:   10 * time.Second,
            ExpectContinueTimeout: 1 * time.Second,
        },
    }, nil
}

// 创建 SS 代理 URL
func createSSProxyURL(node *Node) string {
    params := url.Values{}
    for k, v := range node.params {
        params.Set(k, v)
    }
    return fmt.Sprintf("ss://%s@%s:%s?%s", node.params["password"], node.server, node.port, params.Encode())
}

// 创建 Trojan 代理 URL
func createTrojanProxyURL(node *Node) string {
    params := url.Values{}
    for k, v := range node.params {
        params.Set(k, v)
    }
    return fmt.Sprintf("trojan://%s@%s:%s?%s", node.params["password"], node.server, node.port, params.Encode())
}

// 创建 Vmess 代理 URL
func createVmessProxyURL(node *Node) string {
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
    configJSON, _ := json.Marshal(vmessConfig)
    return fmt.Sprintf("vmess://%s", base64.StdEncoding.EncodeToString(configJSON))
}

// 解析节点
func parseNode(line string) *Node {
    parts := strings.SplitN(line, "=", 2)
    if len(parts) != 2 {
        return nil
    }

    name := strings.TrimSpace(parts[0])
    value := strings.TrimSpace(parts[1])

    // 解析节点配置
    node := &Node{
        params: make(map[string]string),
    }

    // 解析协议
    if strings.HasPrefix(value, "ss://") {
        node.protocol = "ss"
        if err := parseSSNode(value, node); err != nil {
            return nil
        }
    } else if strings.HasPrefix(value, "trojan://") {
        node.protocol = "trojan"
        if err := parseTrojanNode(value, node); err != nil {
            return nil
        }
    } else if strings.HasPrefix(value, "vmess://") {
        node.protocol = "vmess"
        if err := parseVmessNode(value, node); err != nil {
            return nil
        }
    } else {
        return nil
    }

    node.fullName = name
    return node
}

// 解析 SS 节点
func parseSSNode(urlStr string, node *Node) error {
    // 移除 ss:// 前缀
    urlStr = strings.TrimPrefix(urlStr, "ss://")
    
    // 解析 URL
    u, err := url.Parse(urlStr)
    if err != nil {
        return err
    }

    // 解析服务器地址和端口
    host, port, err := net.SplitHostPort(u.Host)
    if err != nil {
        return err
    }

    node.server = host
    node.port = port

    // 解析参数
    query := u.Query()
    for k, v := range query {
        if len(v) > 0 {
            node.params[k] = v[0]
        }
    }

    return nil
}

// 解析 Trojan 节点
func parseTrojanNode(urlStr string, node *Node) error {
    // 移除 trojan:// 前缀
    urlStr = strings.TrimPrefix(urlStr, "trojan://")
    
    // 解析 URL
    u, err := url.Parse(urlStr)
    if err != nil {
        return err
    }

    // 解析服务器地址和端口
    host, port, err := net.SplitHostPort(u.Host)
    if err != nil {
        return err
    }

    node.server = host
    node.port = port

    // 解析参数
    query := u.Query()
    for k, v := range query {
        if len(v) > 0 {
            node.params[k] = v[0]
        }
    }

    return nil
}

// 解析 Vmess 节点
func parseVmessNode(urlStr string, node *Node) error {
    // 移除 vmess:// 前缀
    urlStr = strings.TrimPrefix(urlStr, "vmess://")
    
    // Base64 解码
    data, err := base64.StdEncoding.DecodeString(urlStr)
    if err != nil {
        return err
    }

    // 解析 JSON
    var config map[string]interface{}
    if err := json.Unmarshal(data, &config); err != nil {
        return err
    }

    // 设置基本参数
    node.server = config["add"].(string)
    node.port = fmt.Sprintf("%v", config["port"])
    node.params["uuid"] = config["id"].(string)
    node.params["alterId"] = fmt.Sprintf("%v", config["aid"])
    node.params["network"] = config["net"].(string)
    node.params["host"] = config["host"].(string)
    node.params["path"] = config["path"].(string)
    node.params["tls"] = config["tls"].(string)

    return nil
}

// 强制更新
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

// 获取所有节点
func fetchAllNodes() ([]*Node, error) {
    var allNodes []*Node
    var mu sync.Mutex
    var wg sync.WaitGroup
    errChan := make(chan error, len(subscriptions))

    for _, sub := range subscriptions {
        wg.Add(1)
        go func(s Subscription) {
            defer wg.Done()
            nodes, err := fetchSubscription(s)
            if err != nil {
                errChan <- fmt.Errorf("获取订阅失败 %s: %v", s.airportName, err)
                return
            }
            mu.Lock()
            allNodes = append(allNodes, nodes...)
            mu.Unlock()
        }(sub)
    }

    wg.Wait()
    close(errChan)

    // 检查是否有错误
    for err := range errChan {
        writeLog("ERROR", err.Error(), 0, nil, 0)
    }

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
    timeout := time.After(5 * time.Second)

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
                select {
                case successChan <- struct{}{}:
                case <-timeout:
                }
            } else if err != nil {
                select {
                case errChan <- err:
                case <-timeout:
                }
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
    for {
        select {
        case <-successChan:
            successCount++
        case <-timeout:
            if successCount == 0 {
                var errs []string
                for err := range errChan {
                    errs = append(errs, err.Error())
                }
                return nil, fmt.Errorf("DNS 查询超时: %s", strings.Join(errs, "; "))
            }
            return ips, nil
        case <-errChan:
            // 继续等待成功
        }
    }
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

// 处理节点
func processNodes(nodes []*Node, params url.Values) string {
    var results []string
    var mu sync.Mutex
    var wg sync.WaitGroup
    errChan := make(chan error, len(nodes))

    // 添加代理部分标记
    results = append(results, "[Proxy]")

    for _, node := range nodes {
        wg.Add(1)
        go func(n *Node) {
            defer wg.Done()

            // 处理域名
            processedNodes, err := processNodeDomain(n)
            if err != nil {
                errChan <- fmt.Errorf("处理域名失败 %s: %v", n.server, err)
                return
            }

            // 处理每个节点
            for _, processedNode := range processedNodes {
                // 处理 SNI
                if err := processNodeSNI(processedNode); err != nil {
                    errChan <- fmt.Errorf("处理 SNI 失败 %s: %v", processedNode.server, err)
                    continue
                }

                // 获取国家信息
                country, emoji, err := getNodeCountry(processedNode)
                if err != nil {
                    errChan <- fmt.Errorf("获取国家信息失败 %s: %v", processedNode.server, err)
                    continue
                }

                // 更新节点名称
                processedNode.fullName = fmt.Sprintf("%s %s %s", processedNode.fullName, emoji, country)

                // 构建节点字符串
                nodeStr := buildNodeString(processedNode, params)

                mu.Lock()
                results = append(results, nodeStr)
                mu.Unlock()
            }
        }(node)
    }

    wg.Wait()
    close(errChan)

    // 检查错误
    for err := range errChan {
        writeLog("ERROR", err.Error(), 0, nil, 0)
    }

    // 添加规则部分
    results = append(results, "\n[Rule]")
    results = append(results, "FINAL,DIRECT")

    return strings.Join(results, "\n")
}

// 构建节点字符串
func buildNodeString(node *Node, params url.Values) string {
    var parts []string

    // 添加基本参数
    switch node.protocol {
    case "ss":
        parts = append(parts, fmt.Sprintf("%s = ss, %s, %s, encrypt-method=%s, password=%s",
            node.fullName, node.server, node.port, node.params["method"], node.params["password"]))
    case "trojan":
        parts = append(parts, fmt.Sprintf("%s = trojan, %s, %s, password=%s",
            node.fullName, node.server, node.port, node.params["password"]))
    case "vmess":
        parts = append(parts, fmt.Sprintf("%s = vmess, %s, %s, username=%s, ws=true, tls=true",
            node.fullName, node.server, node.port, node.params["uuid"]))
    }

    // 添加其他参数
    for k, v := range node.params {
        if k != "method" && k != "password" && k != "uuid" {
            parts = append(parts, fmt.Sprintf("%s=%s", k, v))
        }
    }

    // 添加 URL 参数
    for k, v := range params {
        if mappedParam, ok := paramMap[k]; ok {
            parts = append(parts, fmt.Sprintf("%s=%s", mappedParam, v[0]))
        }
    }

    return strings.Join(parts, ", ")
} 