package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func startServer() error {
	srv := &http.Server{
		Addr:         ":8080",
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
		Handler:      http.HandlerFunc(handleFusion),
	}
	
	// 优雅关闭
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan
		
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("HTTP server关闭错误: %v", err)
		}
	}()
	
	return srv.ListenAndServe()
}

func handleFusion(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		log.Printf("%s %s %s %v", r.RemoteAddr, r.Method, r.URL.Path, time.Since(start))
	}()
	// 设置CORS响应头
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")

	// 处理OPTIONS请求
	if r.Method == "OPTIONS" {
		return
	}

	// 验证TOKEN
	if r.URL.Query().Get("t") != TOKEN {
		http.Error(w, "无效的TOKEN", http.StatusUnauthorized)
		return
	}

	// 处理强制更新请求
	if r.URL.Query().Get("f") != "" {
		go func() {
			if err := updateNodes(); err != nil {
				log.Printf("强制更新失败: %v", err)
			}
		}()
		w.WriteHeader(http.StatusAccepted) // 202
		return
	}

	// 读取节点配置
	nodePath := filepath.Join(fusionDir, "node.conf")
	content, err := os.ReadFile(nodePath)
	if err != nil {
		if os.IsNotExist(err) {
			// 配置不存在，触发更新
			go func() {
				if err := updateNodes(); err != nil {
					log.Printf("更新节点失败: %v", err)
				}
			}()
			w.WriteHeader(http.StatusNoContent) // 204
			return
		}
		http.Error(w, "读取配置失败", http.StatusInternalServerError)
		return
	}

	// 处理URL参数
	nodes := processNodes(string(content), r.URL.Query())

	// 返回处理后的节点
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(nodes))
}

func processNodes(content string, params map[string][]string) string {
	lines := strings.Split(content, "\n")
	processed := make([]string, 0, len(lines))

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// 处理节点参数
		if udp := params["udp"]; len(udp) > 0 {
			line = updateParameter(line, "udp-relay", udp[0])
		}
		if tfo := params["tfo"]; len(tfo) > 0 {
			line = updateParameter(line, "tfo", tfo[0])
		}
		if quic := params["quic"]; len(quic) > 0 {
			line = updateParameter(line, "block-quic", quic[0])
		}

		processed = append(processed, line)
	}

	return strings.Join(processed, "\n")
}

func updateParameter(line, param, value string) string {
	// 查找参数
	paramIndex := strings.Index(line, param+"=")
	if paramIndex == -1 {
		// 参数不存在，添加新参数
		if strings.HasSuffix(line, ",") {
			return line + " " + param + "=" + value
		}
		return line + ", " + param + "=" + value
	}

	// 找到参数值的开始和结束位置
	start := paramIndex + len(param) + 1
	end := start
	for end < len(line) && line[end] != ',' && line[end] != ' ' {
		end++
	}

	// 替换参数值
	return line[:start] + value + line[end:]
}
