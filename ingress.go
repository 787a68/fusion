package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func processIngressNode(node string) ([]map[string]any, error) {
	node = strings.TrimSpace(node)
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("无效的节点格式: %s", node)
	}
	name, config := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	params, order := parseParams(config)
	proxyType := getProxyType(config)
	if proxyType == "" {
		return nil, fmt.Errorf("不支持的代理类型: %s", config)
	}
	server := params["server"]
	if server == "" {
		return nil, fmt.Errorf("未找到服务器地址")
	}

	// 解析为 map[string]any
	nodeMap := make(map[string]any)
	nodeMap["name"] = name
	for k, v := range params {
		nodeMap[k] = v
	}
	nodeMap["_params"] = params
	nodeMap["_order"] = order

	// DNS 查询
	if net.ParseIP(server) != nil {
		nodeMap["server"] = server
		return []map[string]any{nodeMap}, nil
	}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: 3 * time.Second,
			}
			return d.DialContext(ctx, network, address)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ips, err := resolver.LookupHost(ctx, server)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("DNS解析失败 %s: %v", server, err)
	}
	var result []map[string]any
	for _, ip := range ips {
		m := make(map[string]any)
		for k, v := range nodeMap {
			m[k] = v
		}
		m["server"] = ip
		result = append(result, m)
	}
	return result, nil
}

func getProxyType(config string) string {
	config = strings.ToLower(config)
	proxyTypes := []string{
		"vmess", "trojan", "ss", "ssr", "http", "https", 
		"socks5", "socks5-tls", "snell", "tuic",
		"wireguard", "hysteria", "vless",
	}
	
	for _, typ := range proxyTypes {
		if strings.HasPrefix(config, typ) {
			return typ
		}
	}
	return ""
}

func addSNI(config string, server string) string {
	parts := strings.Split(config, ",")
	parts = append(parts, fmt.Sprintf("sni=%s", server))
	return strings.Join(parts, ",")
}

func parseIngressParams(config string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(config, ",")

	// 处理第一个参数（协议类型）
	if len(parts) > 0 {
		params["type"] = strings.TrimSpace(parts[0])
	}

	// 处理第二个参数（服务器地址）
	if len(parts) > 1 {
		params["server"] = strings.TrimSpace(parts[1])
	}

	// 处理第三个参数（端口）
	if len(parts) > 2 {
		params["port"] = strings.TrimSpace(parts[2])
	}

	// 处理剩余的参数
	for i := 3; i < len(parts); i++ {
		part := strings.TrimSpace(parts[i])
		keyVal := strings.SplitN(part, "=", 2)
		if len(keyVal) == 2 {
			params[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
		}
	}

	return params
}

// 判断协议是否需要 TLS
func needsTLS(proxyType string) bool {
	// 白名单：需要 TLS 的协议
	tlsTypes := map[string]bool{
		"vmess": true,
		"trojan": true,
		"https": true,
		"socks5-tls": true,
		"snell": true,
		"tuic": true,
		"hysteria": true,
		"vless": true,
	}
	return tlsTypes[proxyType]
}
