package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func processIngressNode(node string) (string, error) {
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的节点格式")
	}

	name, config := parts[0], parts[1]
	params := parseParams(config)

	// 判断代理类型
	proxyType := getProxyType(config)
	if proxyType == "" {
		return node, nil
	}

	// 获取服务器地址
	server := params["server"]
	if server == "" {
		return node, nil
	}

	// 检查是否为IP地址
	if net.ParseIP(server) != nil {
		return node, nil
	}
	// 执行DNS查询（带超时控制）
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
	if err != nil {
		return "", fmt.Errorf("DNS解析失败 %s: %v", server, err)
	}
	
	if len(ips) == 0 {
		return "", fmt.Errorf("未找到IP地址: %s", server)
	}

	// 如果没有SNI参数，添加原始域名作为SNI
	if params["sni"] == "" {
		config = addSNI(config, server)
	}

	// 替换服务器地址为IP
	nodeList := make([]string, 0, len(ips))
	for _, ip := range ips {
		newConfig := strings.Replace(config, server, ip, 1)
		nodeList = append(nodeList, fmt.Sprintf("%s = %s", name, newConfig))
	}

	return strings.Join(nodeList, "\n"), nil
}

func parseParams(config string) map[string]string {
	params := make(map[string]string)
	parts := strings.Split(config, ",")
	
	for _, part := range parts {
		part = strings.TrimSpace(part)
		keyVal := strings.SplitN(part, "=", 2)
		if len(keyVal) == 2 {
			params[strings.TrimSpace(keyVal[0])] = strings.TrimSpace(keyVal[1])
		}
	}

	return params
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

func addSNI(config, domain string) string {
	if strings.HasSuffix(config, ",") {
		return config + " sni=" + domain
	}
	return config + ", sni=" + domain
}
