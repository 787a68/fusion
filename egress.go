package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/adapter/outbound"
	"github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/tunnel"
	"github.com/Dreamacro/clash/adapter"
)

type NodeInfo struct {
	ISOCode    string
	Flag       string
	NATType    string
	Count      int
	Meta       map[string]any
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

func getEgressInfo(node string) (*NodeInfo, error) {
	// 构建走 mihomo 端口的代理 http.Client
	proxyURL, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", mihomoPort))
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		Timeout: totalTimeout,
	}

	var info NodeInfo
	var errChan = make(chan error, 1)
	var doneChan = make(chan struct{})
	var once sync.Once

	// 设置总超时控制
	go func() {
		time.Sleep(totalTimeout)
		once.Do(func() {
			close(doneChan)
		})
	}()

	// 1. 先执行地理位置检测
	iso, flag, err := getLocationInfo(client)
	if err != nil {
		return nil, fmt.Errorf("地理位置测试失败: %v", err)
	}
	info.ISOCode = iso
	info.Flag = flag

	// 如果不是香港节点，直接返回结果
	if info.ISOCode != "HK" {
		counterMutex.Lock()
		nodeCounter[info.ISOCode]++
		info.Count = nodeCounter[info.ISOCode]
		counterMutex.Unlock()
		return &info, nil
	}

	// 2. 对于香港节点，仅检测 NAT 类型
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		natType, err := getNATType(client)
		if err != nil {
			select {
			case errChan <- fmt.Errorf("获取NAT类型失败: %v", err):
			case <-doneChan:
			}
			return
		}
		info.NATType = natType
	}()

	go func() {
		wg.Wait()
		once.Do(func() {
			close(doneChan)
		})
	}()

	select {
	case <-doneChan:
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

// 通过代理 client 获取地理位置（subs-check 方式）
func getLocationInfo(client *http.Client) (string, string, error) {
	resp, err := client.Get("https://www.cloudflare.com/cdn-cgi/trace")
	if err != nil {
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

// 检查端口是否开放
func isPortOpen(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// DetectNodes 并发检测所有节点的 GEO 和 NAT 类型
func DetectNodes(nodes []string, maxConcurrent int) []*NodeInfo {
	var wg sync.WaitGroup
	results := make([]*NodeInfo, len(nodes))
	tasks := make(chan struct{
		idx int
		node string
	}, len(nodes))

	// 启动 worker
	for i := 0; i < maxConcurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range tasks {
				info, err := getEgressInfo(task.node)
				if err == nil {
					results[task.idx] = info
				} else {
					results[task.idx] = nil // 或可记录错误
				}
			}
		}()
	}

	// 分发任务
	for idx, node := range nodes {
		tasks <- struct{
			idx int
			node string
		}{idx, node}
	}
	close(tasks)
	wg.Wait()
	return results
}

// adapter 机制：每节点独立 client 检测
func getEgressInfoAdapter(meta map[string]any) (*NodeInfo, error) {
	// 1. 生成独立代理 client
	client := CreateAdapterClient(meta)
	if client == nil {
		return nil, fmt.Errorf("adapter client 创建失败")
	}
	defer client.Close()

	// 2. GEO 检测
	iso, flag, err := getLocationInfo(client.Client)
	if err != nil {
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
	// 依赖 subs-check adapter/constant
	proxy, err := adapter.ParseProxy(meta)
	if err != nil {
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
	if pc.proxy != nil {
		pc.proxy.Close()
	}
	pc.Client = nil
}
