package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
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

	mihomoProcess *os.Process
	mihomoPort    = 7890
	mihomoMutex   sync.Mutex
	mihomoStarted bool
)

const (
	// 全局超时设置
	mihomoStartTimeout = 5 * time.Second    // mihomo 启动超时
	proxyUpdateTimeout = 2 * time.Second    // 代理配置更新超时
	locationTimeout    = 3 * time.Second    // 位置信息获取超时
	traceTimeout       = 20 * time.Second   // trace 检测超时
	natTimeout         = 3 * time.Second    // NAT 检测超时
	totalTimeout       = 25 * time.Second   // 总超时时间
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
	// 确保 mihomo 已启动
	if err := startMihomo(); err != nil {
		return nil, fmt.Errorf("启动 mihomo 失败: %v", err)
	}

	// 更新代理配置
	if err := updateMihomoProxy(node); err != nil {
		return nil, fmt.Errorf("更新代理配置失败: %v", err)
	}

	var info NodeInfo
	var wg sync.WaitGroup
	var errChan = make(chan error, 3)
	var doneChan = make(chan struct{})
	var once sync.Once
	var geoDone = make(chan struct{})

	// 设置总超时控制
	go func() {
		time.Sleep(totalTimeout)
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
			case errChan <- fmt.Errorf("地理位置测试失败: %v", err):
			case <-doneChan:
			}
			return
		}
		info.ISOCode = iso
		info.Flag = flag
		close(geoDone)  // 标记地理位置测试完成
	}()

	// 2. 获取trace节点数
	go func() {
		defer wg.Done()
		// 等待地理位置测试完成或失败
		select {
		case <-geoDone:
			// 地理位置测试成功，检查是否为香港
			if info.ISOCode != "HK" {
				return // 不是香港，跳过 trace 检测
			}
		case <-doneChan:
			return
		}
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
		// 等待地理位置测试完成或失败
		select {
		case <-geoDone:
			// 地理位置测试成功，检查是否为香港
			if info.ISOCode != "HK" {
				return // 不是香港，跳过 NAT 检测
			}
		case <-doneChan:
			return
		}
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
	case <-time.After(totalTimeout):
		return nil, fmt.Errorf("获取节点信息超时")
	}
}

func getLocationInfo(node string) (string, string, error) {
	// 使用代理获取出口IP
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: func(_ *http.Request) (*url.URL, error) {
				return url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mihomoPort))
			},
		},
		Timeout: locationTimeout,
	}

	// 获取出口IP
	resp, err := client.Get("https://api.ipify.org")
	if err != nil {
		return "", "", fmt.Errorf("获取出口IP失败: %v", err)
	}
	defer resp.Body.Close()

	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("读取响应失败: %v", err)
	}

	// 获取地理位置信息
	location, err := getLocationFromIP(string(ip))
	if err != nil {
		return "", "", fmt.Errorf("获取地理位置信息失败: %v", err)
	}

	// 解析位置信息
	parts := strings.Split(location, " ")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("无效的位置信息格式")
	}

	country := parts[0]
	code := getCountryCode(country)
	flag := getCountryFlag(code)

	return code, flag, nil
}

func getTraceCount(node string) (int, error) {
	// 使用代理执行 trace 命令
	ctx, cancel := context.WithTimeout(context.Background(), traceTimeout)
	defer cancel()

	// 增加 tracert 的超时参数
	cmd := exec.CommandContext(ctx, "tracert", "-d", "-h", "15", "-w", "1000", "8.8.8.8")
	cmd.Env = append(os.Environ(), fmt.Sprintf("http_proxy=http://127.0.0.1:%d", mihomoPort))
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return 0, fmt.Errorf("trace 检测超时")
		}
		return 0, fmt.Errorf("执行 trace 失败: %v", err)
	}

	// 计算节点数
	lines := strings.Split(string(output), "\n")
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "ms") {
			count++
		}
	}

	return count, nil
}

func getNATType(node string) (string, error) {
	// 使用代理执行 NAT 检测
	ctx, cancel := context.WithTimeout(context.Background(), natTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "stunclient", "stun.l.google.com", "19302")
	cmd.Env = append(os.Environ(), fmt.Sprintf("http_proxy=http://127.0.0.1:%d", mihomoPort))
	
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "Unknown", fmt.Errorf("NAT 检测超时")
		}
		return "Unknown", fmt.Errorf("执行 NAT 检测失败: %v", err)
	}

	// 解析 NAT 类型
	if strings.Contains(string(output), "Full Cone") {
		return "FullCone", nil
	} else if strings.Contains(string(output), "Restricted Cone") {
		return "RestrictedCone", nil
	} else if strings.Contains(string(output), "Port Restricted Cone") {
		return "PortRestrictedCone", nil
	} else if strings.Contains(string(output), "Symmetric") {
		return "Symmetric", nil
	}

	return "Unknown", nil
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

// 启动 mihomo 代理服务器
func startMihomo() error {
	mihomoMutex.Lock()
	defer mihomoMutex.Unlock()

	if mihomoStarted {
		return nil
	}

	// 创建基础配置文件
	config := fmt.Sprintf(`{
		"port": %d,
		"socks-port": %d,
		"allow-lan": true,
		"mode": "rule",
		"log-level": "info",
		"external-controller": "127.0.0.1:%d",
		"proxies": [],
		"proxy-groups": [
			{
				"name": "proxy",
				"type": "select",
				"proxies": ["proxy"]
			}
		],
		"rules": [
			"MATCH,proxy"
		]
	}`, mihomoPort, mihomoPort+1, mihomoPort+2)

	// 创建临时配置文件
	tmpFile, err := os.CreateTemp("", "mihomo-*.yaml")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	// 写入配置
	if _, err := tmpFile.WriteString(config); err != nil {
		return fmt.Errorf("写入配置文件失败: %v", err)
	}
	tmpFile.Close()

	// 启动 mihomo
	cmd := exec.Command("mihomo", "-f", tmpFile.Name())
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 mihomo 失败: %v", err)
	}

	mihomoProcess = cmd.Process

	// 等待 mihomo 启动，使用更短的检查间隔
	for i := 0; i < 10; i++ {
		if isPortOpen(mihomoPort) && isPortOpen(mihomoPort+2) {
			mihomoStarted = true
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("mihomo 启动超时")
}

// 更新 mihomo 代理配置
func updateMihomoProxy(node string) error {
	// 确保 mihomo 已启动
	if err := startMihomo(); err != nil {
		return err
	}

	// 构建完整的代理配置
	proxyConfig := fmt.Sprintf(`{
		"proxies": [
			%s
		],
		"proxy-groups": [
			{
				"name": "proxy",
				"type": "select",
				"proxies": ["proxy"]
			}
		],
		"rules": [
			"MATCH,proxy"
		]
	}`, node)

	// 通过 API 更新配置，添加超时控制
	client := &http.Client{
		Timeout: proxyUpdateTimeout,
	}
	
	resp, err := client.Put(
		fmt.Sprintf("http://127.0.0.1:%d/configs", mihomoPort+2),
		"application/json",
		strings.NewReader(proxyConfig),
	)
	if err != nil {
		return fmt.Errorf("更新代理配置失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("更新代理配置失败，状态码: %d, 响应: %s", resp.StatusCode, string(body))
	}

	// 等待配置生效
	time.Sleep(500 * time.Millisecond)

	return nil
}

// 清理资源
func cleanup() {
	if mihomoProcess != nil {
		mihomoProcess.Kill()
		mihomoProcess = nil
		mihomoStarted = false
	}
}

// 从IP获取地理位置信息
func getLocationFromIP(ip string) (string, error) {
	// 检查缓存
	if info, ok := locationCache.Load(ip); ok {
		return info.(string), nil
	}

	// 使用 ip-api.com 获取地理位置信息
	resp, err := http.Get(fmt.Sprintf("http://ip-api.com/json/%s", ip))
	if err != nil {
		return "", fmt.Errorf("请求 ip-api.com 失败: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Country     string `json:"country"`
		CountryCode string `json:"countryCode"`
		Region      string `json:"region"`
		City        string `json:"city"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("解析响应失败: %v", err)
	}

	// 构建位置信息
	location := fmt.Sprintf("%s %s", result.Country, result.City)
	
	// 缓存结果
	locationCache.Store(ip, location)
	
	return location, nil
}
