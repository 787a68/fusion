package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

func processIngressNode(node string) (string, error) {
	// 移除可能的 BOM 标记和空格
	node = strings.TrimSpace(node)
	
	// 处理节点格式
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的节点格式: %s", node)
	}

	name, config := parts[0], strings.TrimSpace(parts[1])
	
	// 清理名称中的特殊字符
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "[]")
	
	// 解析配置参数
	params := parseIngressParams(config)

	// 判断代理类型
	proxyType := getProxyType(config)
	if proxyType == "" {
		return "", fmt.Errorf("不支持的代理类型: %s", config)
	}

	// 获取服务器地址
	server := params["server"]
	if server == "" {
		return "", fmt.Errorf("未找到服务器地址")
	}

	// 检查是否为IP地址
	if net.ParseIP(server) != nil {
		// 如果是IP地址，直接返回处理后的节点
		if params["sni"] == "" {
			config = addSNI(config, server)
		}
		return fmt.Sprintf("%s = %s", name, config), nil
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
