package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func updateNodes() error {
	mu.Lock()
	defer mu.Unlock()

	// 获取订阅链接
	subs := strings.TrimSpace(os.Getenv("SUB"))
	if subs == "" {
		return fmt.Errorf("未设置SUB环境变量")
	}

	// 拆分订阅链接
	subList := strings.Split(subs, "||")
	if len(subList) == 0 {
		return fmt.Errorf("SUB环境变量格式错误")
	}
	
	nodes := make(map[string][]string)
	var nodesMutex sync.Mutex

	// 并行获取节点
	var wg sync.WaitGroup
	for _, sub := range subList {
		// 处理订阅链接格式
		var name, url string
		if strings.Contains(sub, "=") {
			parts := strings.SplitN(sub, "=", 2)
			if len(parts) != 2 {
				log.Printf("无效的订阅格式: %s", sub)
				continue
			}
			name, url = parts[0], parts[1]
		} else {
			// 如果没有指定名称，使用默认名称
			name = "Default"
			url = sub
		}

		// 验证URL格式
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			log.Printf("无效的URL格式: %s", url)
			continue
		}

		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			if subNodes, err := fetchSubscription(url); err != nil {
				log.Printf("获取订阅失败 %s: %v", name, err)
			} else {
				nodesMutex.Lock()
				nodes[name] = subNodes
				nodesMutex.Unlock()
			}
		}(name, url)
	}
	wg.Wait()

	// 添加自定义节点
	if customNodes := os.Getenv("NODE"); customNodes != "" {
		nodes["Custom"] = strings.Split(customNodes, "\n")
	}

	// 处理所有节点
	allNodes := make([]string, 0)
	for source, sourceNodes := range nodes {
		for _, node := range sourceNodes {
			// 跳过空节点
			if strings.TrimSpace(node) == "" {
				continue
			}
			
			processedNode, err := processNode(source, node)
			if err != nil {
				log.Printf("处理节点失败 [%s]: %v", node, err)
				continue
			}
			allNodes = append(allNodes, processedNode)
		}
	}

	// 检查是否成功处理了任何节点
	if len(allNodes) == 0 {
		return fmt.Errorf("没有成功处理任何节点")
	}

	// 检查节点内容是否为空
	content := strings.Join(allNodes, "\n")
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("生成的节点配置为空")
	}

	// 写入配置文件
	nodePath := filepath.Join(fusionDir, "node.conf")
	return os.WriteFile(nodePath, []byte(content), 0644)
}

func fetchSubscription(url string) ([]string, error) {
	var lastErr error
	for retries := 0; retries < 3; retries++ {
		if retries > 0 {
			time.Sleep(time.Duration(retries) * time.Second)
		}
		
		client := &http.Client{
			Timeout: 5 * time.Second,
		}

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}

		req.Header.Set("User-Agent", "Surge")
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		// 限制响应大小为10MB
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if err != nil {
			lastErr = err
			continue
		}
				if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("HTTP状态码错误: %d", resp.StatusCode)
			continue
		}
		
		content := string(body)
		// 提取[Proxy]部分，支持大小写
		startIdx := -1
		for _, header := range []string{"[Proxy]", "[proxy]", "[PROXY]"} {
			if idx := strings.Index(content, header); idx != -1 {
				startIdx = idx + len(header)
				break
			}
		}
		if startIdx == -1 {
			return nil, fmt.Errorf("未找到[Proxy]部分")
		}

		// 查找下一个Section
		endIdx := len(content)  // 初始化为内容长度
		for _, section := range []string{"[Rule]", "[RULE]", "[Proxy Group]", "[PROXY-GROUP]"} {
			if idx := strings.Index(content[startIdx:], section); idx != -1 {
				if startIdx+idx < endIdx {
					endIdx = startIdx + idx
				}
			}
		}

		// 确保切片范围有效
		if startIdx >= endIdx {
			return nil, fmt.Errorf("无效的配置格式：无法找到有效的节点部分")
		}

		content = content[startIdx:endIdx]

		// 过滤节点
		var nodes []string
		for _, line := range strings.Split(content, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.Contains(line, "reject") || strings.Contains(line, "direct") {
				continue
			}
			nodes = append(nodes, line)
		}

		// 确保至少有一个有效节点
		if len(nodes) == 0 {
			return nil, fmt.Errorf("未找到有效的节点配置")
		}

		return nodes, nil
	}
	return nil, lastErr
}

func processNode(source, node string) (string, error) {
	// 预处理节点，获取域名IP
	processedNodes, err := processIngressNode(node)
	if err != nil {
		return "", fmt.Errorf("获取位置信息失败: %v", err)
	}

	// 处理所有解析出的节点
	nodeList := strings.Split(processedNodes, "\n")
	if len(nodeList) == 0 {
		return "", fmt.Errorf("处理节点后未得到有效节点")
	}

	// 处理每个节点
	var processedNodeList []string
	for _, processedNode := range nodeList {
		// 获取节点信息
		info, err := getEgressInfo(processedNode)
		if err != nil {
			log.Printf("获取节点信息失败 [%s]: %v", processedNode, err)
			continue
		}

		// 重命名节点
		parts := strings.SplitN(processedNode, "=", 2)
		if len(parts) != 2 {
			log.Printf("无效的节点格式: %s", processedNode)
			continue
		}

		// 转换NAT类型为字母
		natType := "D" // Unknown
		switch info.NATType {
		case "FullCone":
			natType = "A"
		case "RestrictedCone":
			natType = "B"
		case "PortRestrictedCone":
			natType = "C"
		case "Symmetric":
			natType = "D"
		}

		// 格式化节点名称: {机场名} {iso二字代码}{旗帜emoji}-T{trace节点数}🔀{nat类型字母}-{两位计数编号}
		newName := fmt.Sprintf("%s %s%s-T%d🔀%s-%02d",
			strings.TrimSpace(source),
			strings.ToUpper(info.ISOCode),
			info.Flag,
			info.TraceCount,
			natType,
			info.Count)

		// 转换布尔值
		config := strings.TrimSpace(parts[1])
		config = strings.ReplaceAll(config, "true", "1")
		config = strings.ReplaceAll(config, "false", "0")

		processedNodeList = append(processedNodeList, fmt.Sprintf("%s = %s", newName, config))
	}

	// 如果所有节点都处理失败，返回错误
	if len(processedNodeList) == 0 {
		return "", fmt.Errorf("所有节点处理失败")
	}

	// 返回所有成功处理的节点
	return strings.Join(processedNodeList, "\n"), nil
}
