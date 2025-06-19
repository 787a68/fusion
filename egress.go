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

// 通过代理 client 获取 NAT 类型
func getNATType(client *http.Client) (string, error) {
	// 这里假设有 NAT 检测 API 或 STUN 服务，返回类型在 header 或 body
	ctx, cancel := context.WithTimeout(context.Background(), natTimeout)
	defer cancel()
	// 示例用法，实际可替换为你的 NAT 检测 API
	req, err := http.NewRequestWithContext(ctx, "GET", "https://stun.l.google.com:19302", nil)
	if err != nil {
		return "Unknown", err
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("getNATType 执行 STUN 检测失败: %v", err)
		return "Unknown", fmt.Errorf("执行 STUN 检测失败: %v", err)
	}
	defer resp.Body.Close()
	// 解析 NAT 类型（示例，实际请根据你的 API 返回格式调整）
	var natType string
	if resp.Header.Get("X-NAT-Type") == "Full Cone" {
		natType = "FullCone"
	} else if resp.Header.Get("X-NAT-Type") == "Restricted Cone" {
		natType = "RestrictedCone"
	} else if resp.Header.Get("X-NAT-Type") == "Port Restricted Cone" {
		natType = "PortRestrictedCone"
	} else if resp.Header.Get("X-NAT-Type") == "Symmetric" {
		natType = "Symmetric"
	} else {
		natType = "Unknown"
	}
	return natType, nil
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
		switch k {
		case "encrypt-method":
			adapted["cipher"] = v
		case "tfo":
			adapted["tcp-fast-open"] = v
		case "udp-relay":
			adapted["udp"] = v
		default:
			adapted[k] = v
		}
	}
	return adapted
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
