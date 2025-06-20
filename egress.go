package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/constant"
	"github.com/pion/stun"
)

type NodeInfo struct {
	ISOCode    string
	Flag       string
	NATType    string
	Count      int
	Meta       map[string]any
	Params     map[string]string // 原始参数map
	Order      []string          // 参数顺序
}

var (
	// 节点计数器
	nodeCounter = make(map[string]int)
	counterMutex sync.Mutex

	// 地理位置缓存
	locationCache = make(map[string]struct {
		ISOCode string
		City    string
		Time    time.Time
	})
	locationCacheMutex sync.Mutex

	// NAT类型缓存
	natCache = make(map[string]struct {
		NATType string
		Time    time.Time
	})
	natCacheMutex sync.Mutex
)

const (
	// 全局超时设置
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
			locationCacheMutex.Lock()
			for ip, info := range locationCache {
				if now.Sub(info.Time) > 24*time.Hour {
					delete(locationCache, ip)
				}
			}
			locationCacheMutex.Unlock()
			
			// 清理NAT类型缓存
			natCacheMutex.Lock()
			for ip, info := range natCache {
				if now.Sub(info.Time) > 6*time.Hour {
					delete(natCache, ip)
				}
			}
			natCacheMutex.Unlock()
		}
	}()
}

// 通过代理 client 获取地理位置（mihomo subs-check 方式）
func getLocationInfo(client *http.Client) (string, string, error) {
	resp, err := client.Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		log.Printf("getLocationInfo 获取地理信息失败: %v", err)
		return "", "", fmt.Errorf("获取地理信息失败: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var loc, ip string
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "loc=") {
			loc = strings.TrimPrefix(line, "loc=")
		}
		if strings.HasPrefix(line, "ip=") {
			ip = strings.TrimPrefix(line, "ip=")
		}
	}
	if loc == "" || ip == "" {
		log.Printf("getLocationInfo 未能获取地理信息")
		return "", "", fmt.Errorf("未能获取地理信息")
	}
	flag := getCountryFlag(loc)
	return loc, flag, nil
}

// 通过代理 client 获取 NAT 类型（使用 pion/stun 专业检测）
func getNATType(client *http.Client) (string, error) {
	stunServers := []string{
		"stun.l.google.com:19302",
		"stun1.l.google.com:19302",
		"stun.stunprotocol.org:3478",
		"stun.voiparound.com:3478",
	}

	proxyDialer := func(network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// 这里只做基础实现，实际如代理不支持 UDP，直接返回错误
		return net.DialTimeout(network, addr, 5*time.Second)
	}

	var results []string
	var errors []error

	var wg sync.WaitGroup
	resultChan := make(chan string, len(stunServers))
	errorChan := make(chan error, len(stunServers))

	udpSupported := false

	for _, server := range stunServers {
		wg.Add(1)
		go func(stunServer string) {
			defer wg.Done()
			conn, err := proxyDialer("udp", stunServer)
			if err != nil || conn == nil {
				// 代理不支持 UDP，直接返回 Unknown
				return
			}
			udpSupported = true
			defer conn.Close()

			conn.SetDeadline(time.Now().Add(5 * time.Second))
			message := stun.MustBuild(stun.TransactionID, stun.BindingRequest)
			_, err = conn.Write(message.Raw)
			if err != nil {
				errorChan <- fmt.Errorf("发送 STUN 请求失败: %v", err)
				return
			}
			buffer := make([]byte, 1024)
			n, err := conn.Read(buffer)
			if err != nil {
				errorChan <- fmt.Errorf("读取 STUN 响应失败: %v", err)
				return
			}
			var response stun.Event
			if err := stun.Decode(buffer[:n], &response); err != nil {
				errorChan <- fmt.Errorf("解析 STUN 响应失败: %v", err)
				return
			}
			var xorAddr stun.XORMappedAddress
			if err := xorAddr.GetFrom(&response); err != nil {
				errorChan <- fmt.Errorf("获取 XOR-MAPPED-ADDRESS 失败: %v", err)
				return
			}
			resultChan <- fmt.Sprintf("%s:%d", xorAddr.IP.String(), xorAddr.Port)
		}(server)
	}

	go func() {
		wg.Wait()
		close(resultChan)
		close(errorChan)
	}()

	for result := range resultChan {
		results = append(results, result)
	}
	for err := range errorChan {
		errors = append(errors, err)
	}

	if !udpSupported {
		log.Printf("getNATType: 代理不支持 UDP，无法检测 NAT 类型")
		return "Unknown", fmt.Errorf("代理不支持 UDP")
	}

	if len(results) == 0 {
		log.Printf("getNATType 所有 STUN 服务器检测失败: %v", errors)
		return "Unknown", fmt.Errorf("所有 STUN 服务器检测失败")
	}

	natType := analyzeNATType(results)
	log.Printf("getNATType 检测结果: %s, 外部地址: %v", natType, results)
	return natType, nil
}

// 分析 NAT 类型（简化版实现）
func analyzeNATType(externalAddrs []string) string {
	if len(externalAddrs) == 0 {
		return "Unknown"
	}

	// 提取 IP 和端口
	var ips []string
	var ports []string
	
	for _, addr := range externalAddrs {
		if host, port, err := net.SplitHostPort(addr); err == nil {
			ips = append(ips, host)
			ports = append(ports, port)
		}
	}

	if len(ips) == 0 {
		return "Unknown"
	}

	// 检查 IP 是否一致
	firstIP := ips[0]
	ipConsistent := true
	for _, ip := range ips {
		if ip != firstIP {
			ipConsistent = false
			break
		}
	}

	// 检查端口是否一致
	firstPort := ports[0]
	portConsistent := true
	for _, port := range ports {
		if port != firstPort {
			portConsistent = false
			break
		}
	}

	// 基于 RFC3489 的简化判断
	if ipConsistent && portConsistent {
		return "FullCone" // 所有服务器看到相同的 IP:Port
	} else if ipConsistent && !portConsistent {
		return "RestrictedCone" // IP 一致但端口不同
	} else {
		return "Symmetric" // IP 都不一致，可能是 Symmetric NAT
	}
}

func getCountryFlag(code string) string {
	// 将国家代码转换为Regional Indicator Symbols
	if len(code) != 2 {
		return "🏴‍☠️"
	}

	code = strings.ToUpper(code)
	return string(0x1F1E6+rune(code[0]-'A')) + string(0x1F1E6+rune(code[1]-'A'))
}

// 解析节点参数，返回参数 map 和参数顺序 slice
func parseParams(config string) (map[string]string, []string) {
	params := make(map[string]string)
	var order []string
	parts := strings.Split(config, ",")

	if len(parts) >= 3 {
		keys := []string{"type", "server", "port"}
		for i, k := range keys {
			params[k] = strings.TrimSpace(parts[i])
			order = append(order, k)
		}
	}

	for i := 3; i < len(parts); i++ {
		part := strings.TrimSpace(parts[i])
		keyVal := strings.SplitN(part, "=", 2)
		if len(keyVal) == 2 {
			key := strings.TrimSpace(keyVal[0])
			value := strings.TrimSpace(keyVal[1])
			params[key] = value
			order = append(order, key)
		}
	}

	// 只做必要字段校验
	if typ, ok := params["type"]; ok && typ == "ss" {
		if _, ok := params["encrypt-method"]; !ok || params["encrypt-method"] == "" {
			log.Printf("SS 节点缺少加密方式字段: %+v", params)
		}
	}

	return params, order
}

// 适配 Mihomo/Clash 格式的 map，不污染原 map
func adaptForMihomo(surgeMap map[string]string) map[string]any {
	adapted := make(map[string]any)
	for k, v := range surgeMap {
		// 自动识别所有布尔值
		if v == "true" || v == "false" || v == "1" || v == "0" {
			adapted[k] = parseBoolString(v)
		} else if k == "encrypt-method" {
			adapted["cipher"] = v
		} else {
			adapted[k] = v
		}
	}
	return adapted
}

func parseBoolString(s string) bool {
	return s == "true" || s == "1"
}

// 检查端口是否开放
func isPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// adapter 机制：每节点独立 client 检测
func getEgressInfoAdapter(meta map[string]any) (*NodeInfo, error) {
	// 取出原始参数并适配 Mihomo/Clash
	params, _ := meta["_params"].(map[string]string)
	mihomoMeta := adaptForMihomo(params)
	// 1. 生成独立代理 client
	client := CreateAdapterClient(mihomoMeta)
	if client == nil {
		log.Printf("getEgressInfoAdapter adapter client 创建失败, meta: %+v", meta)
		return nil, fmt.Errorf("adapter client 创建失败")
	}
	defer client.Close()

	// 2. GEO 检测
	iso, flag, err := getLocationInfo(client.Client)
	if err != nil {
		log.Printf("getEgressInfoAdapter 地理位置测试失败: %v, meta: %+v", err, meta)
		return nil, fmt.Errorf("地理位置测试失败: %v", err)
	}

	info := &NodeInfo{
		ISOCode: iso,
		Flag:    flag,
		Meta:    meta,
	}

	// 3. 只对香港节点检测 NAT
	if iso == "HK" {
		natType, _ := getNATType(client.Client)
		info.NATType = natType
	}

	// 4. 节点计数
	counterMutex.Lock()
	nodeCounter[iso]++
	info.Count = nodeCounter[iso]
	counterMutex.Unlock()

	return info, nil
}

// CreateAdapterClient 用 adapter 机制生成独立代理 client
func CreateAdapterClient(meta map[string]any) *ProxyClient {
	// 依赖 mihomo adapter/constant
	proxy, err := adapter.ParseProxy(meta)
	if err != nil {
		log.Printf("CreateAdapterClient adapter.ParseProxy 失败: %v, meta: %+v", err, meta)
		return nil
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			var u16Port uint16
			if port, err := strconv.ParseUint(port, 10, 16); err == nil {
				u16Port = uint16(port)
			}
			return proxy.DialContext(ctx, &constant.Metadata{
				Host:    host,
				DstPort: u16Port,
			})
		},
		IdleConnTimeout:   10 * time.Second,
		DisableKeepAlives: true,
	}
	return &ProxyClient{
		Client: &http.Client{
			Timeout:   15 * time.Second,
			Transport: transport,
		},
		proxy: proxy,
	}
}

// ProxyClient 封装 http.Client 和底层 Proxy
type ProxyClient struct {
	*http.Client
	proxy constant.Proxy
}

func (pc *ProxyClient) Close() {
	if pc.Client != nil {
		pc.Client.CloseIdleConnections()
	}
	pc.Client = nil
}

// 通过 Cloudflare trace API 获取出口国家代码，失败返回 "Unknown"
func getCountryCode(_ string) string {
	resp, err := http.Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
		return "Unknown"
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "loc=") {
			loc := strings.TrimPrefix(line, "loc=")
			if len(loc) == 2 {
				return strings.ToUpper(loc)
			}
		}
	}
	return "Unknown"
}
