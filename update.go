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
	"github.com/facette/natsort"
)

func updateNodes() error {
	mu.Lock()
	defer mu.Unlock()

	// 1. 拉取并解析所有节点
	allNodes, err := FetchAndParseNodes()
	if err != nil {
		log.Printf("updateNodes 节点拉取/解析失败: %v", err)
		return fmt.Errorf("节点拉取/解析失败: %v", err)
	}

	if len(allNodes) == 0 {
		log.Printf("updateNodes 没有成功处理任何节点")
		return fmt.Errorf("没有成功处理任何节点")
	}
	
	// 2. 并发检测节点质量
	checked := DetectNodesAdapter(allNodes, 20)

	// 3. 重命名/筛选节点
	var outputLines []string
	for _, info := range checked {
		if info == nil || !FilterNode(info.Meta, info) {
			continue // 跳过检测失败或不符合筛选条件
		}
		name := RenameNode(info.Meta, info)
		// 从 Meta 取出 _params 和 _order
		params, ok1 := info.Meta["_params"].(map[string]string)
		order, ok2 := info.Meta["_order"].([]string)
		if !ok1 || !ok2 {
			log.Printf("节点参数顺序信息缺失，跳过: %+v", info.Meta)
			continue
		}
		formatBoolParams(params)
		delete(params, "name") // 确保最终输出不含 name 字段
		line := fmt.Sprintf("%s = %s", name, buildSurgeLine(params, order))
		outputLines = append(outputLines, line)
	}

	content := strings.Join(outputLines, "\n")
	// 输出前自然排序
	lines := strings.Split(content, "\n")
	natsort.Sort(lines)
	content = strings.Join(lines, "\n")

	// 统计每个上游机场的成功/失败节点数量（用 allNodes 和 checked 索引一一对应）
	successBySource := make(map[string]int)
	failBySource := make(map[string]int)
	for i, info := range checked {
		source := ""
		if s, ok := allNodes[i]["source"].(string); ok {
			source = s
		}
		if info == nil {
			failBySource[source]++
		} else {
			successBySource[source]++
		}
	}
	logStr := ""
	for source, succ := range successBySource {
		fail := failBySource[source]
		logStr += source + " 成功: " + fmt.Sprint(succ) + ", 失败: " + fmt.Sprint(fail) + "; "
	}
	log.Printf("各上游机场节点统计: %s", logStr)

	log.Printf("成功节点数量: %d，失败节点数量: %d", len(outputLines), len(checked)-len(outputLines))

	if strings.TrimSpace(content) == "" {
		log.Printf("updateNodes 生成的节点配置为空")
		return fmt.Errorf("生成的节点配置为空")
	}

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
			log.Printf("fetchSubscription 创建请求失败: %v", err)
			return nil, err
		}

		req.Header.Set("User-Agent", "Surge")
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("fetchSubscription 读取响应失败: %v", err)
			lastErr = err
			continue
		}
		defer resp.Body.Close()

		// 限制响应大小为10MB
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if err != nil {
			log.Printf("fetchSubscription 读取响应失败: %v", err)
			lastErr = err
			continue
		}
				if resp.StatusCode != http.StatusOK {
			log.Printf("fetchSubscription HTTP状态码错误: %d", resp.StatusCode)
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
			log.Printf("fetchSubscription 未找到[Proxy]部分")
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
			log.Printf("fetchSubscription 无效的配置格式：无法找到有效的节点部分")
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
			log.Printf("fetchSubscription 未找到有效的节点配置")
			return nil, fmt.Errorf("未找到有效的节点配置")
		}

		if lastErr != nil {
			log.Printf("fetchSubscription 最终失败: %v", lastErr)
		}
		return nodes, nil
	}
	return nil, lastErr
}

// map 转 Surge 格式
func mapToSurgeLine(m map[string]any, name string) string {
	typ := m["type"]
	server := m["server"]
	port := m["port"]
	var params []string
	for k, v := range m {
		if k == "name" || k == "type" || k == "server" || k == "port" || k == "source" {
			continue
		}
		params = append(params, fmt.Sprintf("%s=%v", k, v))
	}
	return fmt.Sprintf("%s = %s, %s, %s, %s", name, typ, server, port, strings.Join(params, ", "))
}

// 节点重命名函数
func RenameNode(m map[string]any, info *NodeInfo) string {
	if info.ISOCode == "HK" {
		natType := "U" // 默认 Unknown
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
		return fmt.Sprintf("%s %s%s-🔀%s-%02d", m["source"], strings.ToUpper(info.ISOCode), info.Flag, natType, info.Count)
	}
	return fmt.Sprintf("%s %s%s-%02d", m["source"], strings.ToUpper(info.ISOCode), info.Flag, info.Count)
}

// 节点筛选函数
func FilterNode(m map[string]any, info *NodeInfo) bool {
	if info.ISOCode == "" {
		return false
	}
	// 可扩展更多筛选条件
	return true
}

// 拉取并解析所有节点
func FetchAndParseNodes() ([]map[string]any, error) {
	subs := strings.TrimSpace(os.Getenv("SUB"))
	if subs == "" {
		return nil, fmt.Errorf("未设置SUB环境变量")
	}
	subList := strings.Split(subs, "||")
	nodes := make(map[string][]string)
	var nodesMutex sync.Mutex

	// 并发拉取订阅
	var wg sync.WaitGroup
	for _, sub := range subList {
		var name, url string
		if strings.Contains(sub, "=") {
			parts := strings.SplitN(sub, "=", 2)
			if len(parts) != 2 {
				continue
			}
			name, url = parts[0], parts[1]
		} else {
			name = "Default"
			url = sub
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			continue
		}
		wg.Add(1)
		go func(name, url string) {
			defer wg.Done()
			subNodes, err := fetchSubscription(url)
	if err != nil {
				return
			}
			nodesMutex.Lock()
			nodes[name] = subNodes
			nodesMutex.Unlock()
		}(name, url)
	}
	wg.Wait()

	// 添加自定义节点
	if customNodes := os.Getenv("NODE"); customNodes != "" {
		nodes["Custom"] = strings.Split(customNodes, "\n")
	}

	// 结构化所有节点
	var allNodeMaps []map[string]any
	for source, sourceNodes := range nodes {
		for _, node := range sourceNodes {
			if strings.TrimSpace(node) == "" {
				continue
			}
			nodeMaps, err := processIngressNode(node)
	if err != nil {
				continue
			}
			for _, m := range nodeMaps {
				m["source"] = source
				allNodeMaps = append(allNodeMaps, m)
			}
		}
	}
	return allNodeMaps, nil
}

// 将参数 map 中所有布尔值转换为 "1"/"0" 字符串
// 注意：本函数只允许在最终生成文件前调用，其他任何流程禁止调用布尔值转换！
func formatBoolParams(params map[string]string) {
	for k, v := range params {
		if v == "true" {
			params[k] = "1"
		} else if v == "false" {
			params[k] = "0"
		}
	}
	}

// 按协议类型输出原生 Surge 节点格式，支持多协议
func buildSurgeLine(params map[string]string, order []string) string {
	typeStr := params["type"]
	server := params["server"]
	port := params["port"]
	var mainParts []string
	var extraParts []string

	switch typeStr {
	case "ss":
		mainParts = []string{"ss", server, port}
		if v, ok := params["encrypt-method"]; ok && v != "" {
			extraParts = append(extraParts, "encrypt-method="+v)
		}
		if v, ok := params["password"]; ok && v != "" {
			extraParts = append(extraParts, "password="+v)
	}
	case "trojan":
		mainParts = []string{"trojan", server, port}
		if v, ok := params["password"]; ok && v != "" {
			extraParts = append(extraParts, "password="+v)
		}
		if v, ok := params["sni"]; ok && v != "" {
			extraParts = append(extraParts, "sni="+v)
		}
	case "vmess":
		mainParts = []string{"vmess", server, port}
		if v, ok := params["username"]; ok && v != "" {
			extraParts = append(extraParts, "username="+v)
		}
		for _, k := range []string{"ws", "ws-path", "ws-headers", "tls", "sni", "skip-cert-verify"} {
			if v, ok := params[k]; ok && v != "" {
				extraParts = append(extraParts, k+"="+v)
			}
		}
	default:
		mainParts = []string{typeStr, server, port}
	}

	// 只跳过 type/server/port/name，其余参数全部输出
	exist := map[string]struct{}{"type":{}, "server":{}, "port":{}, "name":{}}
	for _, key := range order {
		if _, skip := exist[key]; skip {
			continue
		}
		if val, ok := params[key]; ok && val != "" {
			extraParts = append(extraParts, key+"="+val)
		}
	}

	mainStr := strings.Join(mainParts, ",") // 主参数间无空格
	extraStr := strings.Join(extraParts, ",")
	if extraStr != "" {
		return mainStr + ", " + extraStr // 参数部分前有逗号+空格
	}
	return mainStr
}
