package main

import (
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