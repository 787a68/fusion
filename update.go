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

	// 拆分订阅链接	subList := strings.Split(subs, "||")
	if len(subList) == 0 {
		return fmt.Errorf("SUB环境变量格式错误")
	}
	
	nodes := make(map[string][]string)

	// 并行获取节点
	var wg sync.WaitGroup
	for _, sub := range subList {
		parts := strings.Split(sub, "=")
		if len(parts) != 2 {
			log.Printf("无效的订阅格式: %s", sub)
			continue
		}

		name, url := parts[0], parts[1]
		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			if subNodes, err := fetchSubscription(url); err != nil {
				log.Printf("获取订阅失败 %s: %v", name, err)
			} else {
				nodes[name] = subNodes
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
			processedNode, err := processNode(source, node)
			if err != nil {
				log.Printf("处理节点失败: %v", err)
				continue
			}
			allNodes = append(allNodes, processedNode)
		}
	}

	// 写入配置文件
	nodePath := filepath.Join(fusionDir, "node.conf")
	return os.WriteFile(nodePath, []byte(strings.Join(allNodes, "\n")), 0644)
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

	// 查找下一个Section		endIdx := len(content)
		for _, section := range []string{"[Rule]", "[RULE]", "[Proxy Group]", "[PROXY-GROUP]"} {
			if idx := strings.Index(content[startIdx:], section); idx != -1 {
				if startIdx+idx < endIdx {
					endIdx = startIdx + idx
				}
			}
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

		return nodes, nil
	}
	return nil, lastErr
}

func processNode(source, node string) (string, error) {
	// 预处理节点，获取域名IP
	node, err := processIngressNode(node)
	if err != nil {
		return "", err
	}

	// 获取节点信息
	info, err := getEgressInfo(node)
	if err != nil {
		return "", err
	}

	// 重命名节点
	parts := strings.SplitN(node, "=", 2)
	if len(parts) != 2 {
		return "", fmt.Errorf("无效的节点格式")
	}
	// 格式化节点名称: {机场名} {iso二字代码}{旗帜emoji}-T{trace节点数}🔀{nat类型}-{两位计数编号}
	newName := fmt.Sprintf("%s %s%s-T%d🔀%s-%02d",
		strings.TrimSpace(source),
		strings.ToUpper(info.ISOCode),
		info.Flag,
		info.TraceCount,
		info.NATType,
		info.Count)

	// 转换布尔值
	config := parts[1]
	config = strings.ReplaceAll(config, "true", "1")
	config = strings.ReplaceAll(config, "false", "0")

	return fmt.Sprintf("%s = %s", newName, config), nil
}
