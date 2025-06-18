package main

import (
	"crypto/rand"
	"encoding/hex"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"sync"
	"time"
)

var (
	TOKEN     string
	Version   = "dev"    // 将被构建时的版本替换
	fusionDir = filepath.Join("/", "fusion")  // 使用filepath.Join确保跨平台兼容
	logDir    string
	mu        sync.Mutex
	logFile   *os.File  // 保存当前日志文件句柄
)

func init() {
	// 确保fusion目录存在
	if err := os.MkdirAll(fusionDir, 0755); err != nil {
		log.Fatalf("创建fusion目录失败: %v", err)
	}

	// 检查并设置TOKEN
	if token := os.Getenv("TOKEN"); token != "" {
		TOKEN = token
	} else {
		// 生成随机TOKEN
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("生成TOKEN失败: %v", err)
		}
		TOKEN = hex.EncodeToString(b)
		// 写入持久存储
		if err := os.WriteFile(filepath.Join(fusionDir, "token"), []byte(TOKEN), 0644); err != nil {
			log.Fatalf("保存TOKEN失败: %v", err)
		}
	}

	// 设置日志目录
	logDir = filepath.Join(fusionDir, "logs")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Fatalf("创建日志目录失败: %v", err)
	}

	// 启动日志系统
	setupLogger()
}

func setupLogger() {
	// 当前日期作为日志文件名
	currentLogFile := filepath.Join(logDir, time.Now().Format("2006-01-02")+".log")
	
	// 如果已经打开了日志文件，且不是今天的，则关闭它
	if logFile != nil {
		logFile.Close()
		logFile = nil
	}
	
	f, err := os.OpenFile(currentLogFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}
	logFile = f
	log.SetOutput(f)
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 启动日志清理协程
	go cleanOldLogs()
	
	// 每天午夜重新设置日志文件
	go func() {
		for {
			now := time.Now()
			next := now.Add(24 * time.Hour)
			next = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, next.Location())
			time.Sleep(next.Sub(now))
			setupLogger()
		}
	}()
}

func cleanOldLogs() {
	for {
		files, err := os.ReadDir(logDir)
		if err != nil {
			log.Printf("读取日志目录失败: %v", err)
			time.Sleep(24 * time.Hour)
			continue
		}

		for _, file := range files {
			if file.IsDir() {
				continue
			}

			name := file.Name()
			if len(name) != 14 { // "2006-01-02.log" length
				continue
			}

			fileDate, err := time.Parse("2006-01-02", name[:10])
			if err != nil {
				continue
			}

			if time.Since(fileDate) > 7*24*time.Hour {
				path := filepath.Join(logDir, name)
				if err := os.Remove(path); err != nil {
					log.Printf("删除过期日志失败 %s: %v", path, err)
				}
			}
		}

		time.Sleep(24 * time.Hour)
	}
}

func checkNodeConfig() {
	for {
		nodePath := filepath.Join(fusionDir, "node.conf")
		info, err := os.Stat(nodePath)
		
		if os.IsNotExist(err) {
			log.Println("node.conf 不存在，执行更新")
			if err := updateNodes(); err != nil {
				log.Printf("更新节点失败: %v", err)
			}
		} else if err == nil {
			// 检查文件是否超过24小时
			if time.Since(info.ModTime()) > 24*time.Hour {
				log.Println("node.conf 超过24小时，执行更新")
				if err := updateNodes(); err != nil {
					log.Printf("更新节点失败: %v", err)
				}
			}
		}

		time.Sleep(2 * time.Hour)
	}
}

func main() {
	log.Printf("Fusion v%s 启动，TOKEN: %s", Version, TOKEN)

	// 捕获关闭信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 启动配置检查
	stopCheck := make(chan struct{})
	go func() {
		checkNodeConfig()
		close(stopCheck)
	}()

	// 启动HTTP服务器
	errChan := make(chan error, 1)
	go func() {
		errChan <- startServer()
	}()

	// 等待信号或错误
	select {
	case <-sigChan:
		log.Println("收到关闭信号，正在gracefully shutdown...")
		// 通知配置检查协程停止
		close(stopCheck)
		// 等待所有goroutine完成
		time.Sleep(1 * time.Second)
	case err := <-errChan:
		log.Fatalf("HTTP服务器启动失败: %v", err)
	}
}
