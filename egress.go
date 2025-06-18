package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

type NodeInfo struct {
	ISOCode    string
	Flag       string
	TraceCount int
	NATType    string
	Count      int
}

var (
	nodeCounter = make(map[string]int)
	counterMutex sync.Mutex

	// 位置信息缓存
	locationCache = make(map[string]struct {
		ISOCode string
		Flag    string
		Time    time.Time
	})
	locationMutex sync.RWMutex
	
	// NAT类型缓存
	natCache = make(map[string]struct {
		Type string
		Time time.Time
	})
	natMutex sync.RWMutex
)

func init() {
	// 定期清理缓存
	go func() {
		for {
			time.Sleep(1 * time.Hour)
			now := time.Now()
			
			// 清理位置信息缓存
			locationMutex.Lock()
			for ip, info := range locationCache {
				if now.Sub(info.Time) > 24*time.Hour {
					delete(locationCache, ip)
				}
			}
			locationMutex.Unlock()
			
			// 清理NAT类型缓存
			natMutex.Lock()
			for ip, info := range natCache {
				if now.Sub(info.Time) > 6*time.Hour {
					delete(natCache, ip)
				}
			}
			natMutex.Unlock()
		}
	}()
}

func getEgressInfo(node string) (*NodeInfo, error) {
	var info NodeInfo
	var wg sync.WaitGroup
	var errChan = make(chan error, 3)
	var doneChan = make(chan struct{})
	var once sync.Once

	// 设置超时控制
	go func() {
		time.Sleep(10 * time.Second)
		once.Do(func() {
			close(doneChan)
		})
	}()

	// 并行执行检测任务
	wg.Add(3)

	// 1. 获取ISO代码和旗帜
	go func() {
		defer wg.Done()
		iso, flag, err := getLocationInfo(node)
		if err != nil {
			select {
			case errChan <- fmt.Errorf("获取位置信息失败: %v", err):
			case <-doneChan:
			}
			return
		}
		info.ISOCode = iso
		info.Flag = flag
	}()

	// 2. 获取trace节点数
	go func() {
		defer wg.Done()
		count, err := getTraceCount(node)
		if err != nil {
			select {
			case errChan <- fmt.Errorf("获取trace信息失败: %v", err):
			case <-doneChan:
			}
			return
		}
		info.TraceCount = count
	}()

	// 3. 获取NAT类型
	go func() {
		defer wg.Done()
		natType, err := getNATType(node)
		if err != nil {
			select {
			case errChan <- fmt.Errorf("获取NAT类型失败: %v", err):
			case <-doneChan:
			}
			return
		}
		info.NATType = natType
	}()

	// 等待所有任务完成或超时
	go func() {
		wg.Wait()
		once.Do(func() {
			close(doneChan)
		})
	}()

	// 设置超时
	select {
	case <-doneChan:
		// 更新节点计数
		counterMutex.Lock()
		nodeCounter[info.ISOCode]++
		info.Count = nodeCounter[info.ISOCode]
		counterMutex.Unlock()
		return &info, nil
	case err := <-errChan:
		return nil, err
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("获取节点信息超时")
	}
}

func getLocationInfo(node string) (string, string, error) {
	// 移除节点名称部分
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("无效的节点格式")
	}

	// 获取配置部分并去除空格
	config := strings.TrimSpace(parts[1])
	
	// 使用 ingress 处理节点
	processedNode, err := processIngressNode(config)
	if err != nil {
		return "", "", fmt.Errorf("处理节点失败: %v", err)
	}

	// 解析处理后的节点配置
	params := parseParams(processedNode)
	serverAddr := params["server"]
	if serverAddr == "" {
		return "", "", fmt.Errorf("未找到服务器地址")
	}

	// 获取代理类型
	proxyType := strings.ToLower(params["type"])
	if proxyType == "" {
		// 如果没有指定类型，尝试从配置中推断
		if strings.HasPrefix(processedNode, "ss://") {
			proxyType = "ss"
		} else if strings.HasPrefix(processedNode, "vmess://") {
			proxyType = "vmess"
		} else if strings.HasPrefix(processedNode, "trojan://") {
			proxyType = "trojan"
		} else if strings.HasPrefix(processedNode, "http://") || strings.HasPrefix(processedNode, "https://") {
			proxyType = "http"
		} else if strings.HasPrefix(processedNode, "socks5://") {
			proxyType = "socks5"
		} else {
			return "", "", fmt.Errorf("不支持的代理类型")
		}
	}

	// 构建代理URL
	var proxyURL string
	switch proxyType {
	case "ss", "ssr", "vmess", "trojan":
		// 这些协议需要完整的配置URL
		proxyURL = processedNode
	case "http", "https":
		proxyURL = fmt.Sprintf("%s://%s", proxyType, serverAddr)
	case "socks5":
		proxyURL = fmt.Sprintf("socks5://%s", serverAddr)
	default:
		return "", "", fmt.Errorf("不支持的代理类型: %s", proxyType)
	}

	// 使用 curl 通过节点查询出口 IP
	cmd := exec.Command("curl", "-s", "--proxy", proxyURL, "https://api.ipify.org?format=json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("获取出口IP失败: %v", err)
	}

	var result struct {
		IP string `json:"ip"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return "", "", fmt.Errorf("解析出口IP失败: %v", err)
	}

	// 检查缓存
	locationMutex.RLock()
	if info, ok := locationCache[result.IP]; ok {
		locationMutex.RUnlock()
		return info.ISOCode, info.Flag, nil
	}
	locationMutex.RUnlock()

	// 使用ipapi.co的免费API查询地理位置
	resp, err := http.Get(fmt.Sprintf("https://ipapi.co/%s/json/", result.IP))
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var locationResult struct {
		CountryCode string `json:"country_code"`
		Error      bool   `json:"error"`
		Reason     string `json:"reason"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&locationResult); err != nil {
		return "", "", err
	}

	if locationResult.Error {
		return "", "", fmt.Errorf("IP API错误: %s", locationResult.Reason)
	}

	// 获取国家代码对应的emoji旗帜
	flag := getCountryFlag(locationResult.CountryCode)

	// 更新缓存
	locationMutex.Lock()
	locationCache[result.IP] = struct {
		ISOCode string
		Flag    string
		Time    time.Time
	}{
		ISOCode: locationResult.CountryCode,
		Flag:    flag,
		Time:    time.Now(),
	}
	locationMutex.Unlock()

	return locationResult.CountryCode, flag, nil
}

func getTraceCount(node string) (int, error) {
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("无效的节点格式")
	}

	// 获取配置部分并去除空格
	config := strings.TrimSpace(parts[1])
	params := parseParams(config)
	ip := params["server"]
	if ip == "" {
		return 0, fmt.Errorf("未找到服务器地址")
	}

	// 根据操作系统选择不同的trace命令
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("tracert", "-h", "30", "-w", "1000", ip)
	default:
		cmd = exec.Command("traceroute", "-m", "30", "-w", "1", ip)
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("trace失败: %v", err)
	}

	// 计算有效跳数
	lines := strings.Split(string(output), "\n")
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "ms") && !strings.Contains(line, "*") {
			count++
		}
	}

	return count, nil
}

func getNATType(node string) (string, error) {
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的节点格式")
	}

	// 获取配置部分并去除空格
	config := strings.TrimSpace(parts[1])
	params := parseParams(config)
	serverAddr := params["server"]
	if serverAddr == "" {
		return "", fmt.Errorf("未找到服务器地址")
	}

	// 检查缓存
	natMutex.RLock()
	if info, ok := natCache[serverAddr]; ok {
		natMutex.RUnlock()
		return info.Type, nil
	}
	natMutex.RUnlock()

	// 创建UDP连接
	conn, err := net.Dial("udp", serverAddr+":3478") // STUN默认端口
	if err != nil {
		return "D", nil // 如果无法建立UDP连接，假设为Symmetric NAT
	}
	defer conn.Close()

	// 发送STUN请求
	// 这里简化了STUN协议的实现，实际应该使用完整的STUN客户端库
	_, err = conn.Write([]byte{0x00, 0x01, 0x00, 0x00}) // 简化的STUN请求
	if err != nil {
		return "C", nil
	}

	// 设置读取超时
	conn.SetReadDeadline(time.Now().Add(time.Second))
	
	// 尝试读取响应
	buf := make([]byte, 1024)
	n, err := conn.Read(buf)
	if err != nil {
		if err, ok := err.(net.Error); ok && err.Timeout() {
			return "B", nil // 超时可能意味着受限的NAT
		}
		return "D", nil
	}

	if n > 0 {
		natType := "A" // 成功收到响应，可能是Full Cone NAT

		// 更新缓存
		natMutex.Lock()
		natCache[serverAddr] = struct {
			Type string
			Time time.Time
		}{Type: natType, Time: time.Now()}
		natMutex.Unlock()

		return natType, nil
	}

	return "C", nil // 默认返回Port Restricted NAT
}

func getCountryFlag(code string) string {
	// 将国家代码转换为Regional Indicator Symbols
	if len(code) != 2 {
		return "🏴‍☠️"
	}

	code = strings.ToUpper(code)
	return string(0x1F1E6+rune(code[0]-'A')) + string(0x1F1E6+rune(code[1]-'A'))
}

func parseParams(config string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(config, ",")

	// 处理协议、服务器和端口
	if len(parts) >= 3 {
		params["type"] = strings.TrimSpace(parts[0])    // 协议类型
		params["server"] = strings.TrimSpace(parts[1])  // 服务器地址
		params["port"] = strings.TrimSpace(parts[2])    // 端口
	}

	// 处理其他参数
	for i := 3; i < len(parts); i++ {
		part := strings.TrimSpace(parts[i])
		keyVal := strings.SplitN(part, "=", 2)
		if len(keyVal) == 2 {
			key := strings.TrimSpace(keyVal[0])
			value := strings.TrimSpace(keyVal[1])
			params[key] = value
		}
	}

	return params
}
