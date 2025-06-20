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
	params["name"] = name
	order = append([]string{"name"}, order...)
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
	seen := make(map[string]struct{}) // ip:port 去重
	for _, ip := range ips {
		port := params["port"]
		key := ip + ":" + port
		if _, exists := seen[key]; exists {
			continue // 跳过重复
		}
		seen[key] = struct{}{}
		m := make(map[string]any)
		for k, v := range nodeMap {
			m[k] = v
		}
		// 深拷贝 params 并替换 server
		newParams := make(map[string]string)
		for k, v := range params {
			newParams[k] = v
		}
		newParams["server"] = ip
		m["server"] = ip
		m["_params"] = newParams
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
