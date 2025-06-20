package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"sync"
	"time"
)

const (
	checkInterval = 2 * time.Hour    // 检查间隔时间
	updateInterval = 24 * time.Hour  // 更新间隔时间
)

var (
	TOKEN     string
	Version   string    // 将被构建时的版本替换
	fusionDir = filepath.Join("/", "data", "fusion")  // 使用 /data/fusion 作为工作目录
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
	tokenPath := filepath.Join(fusionDir, "token")
	if token := os.Getenv("TOKEN"); token != "" {
		TOKEN = token
	} else if tokenBytes, err := os.ReadFile(tokenPath); err == nil {
		// 从文件读取token
		TOKEN = string(tokenBytes)
	} else {
		// 生成随机TOKEN
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			log.Fatalf("生成TOKEN失败: %v", err)
		}
		TOKEN = hex.EncodeToString(b)
		// 写入持久存储
		if err := os.WriteFile(tokenPath, []byte(TOKEN), 0644); err != nil {
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
	// 创建日志文件
	startTime := time.Now().Format("200601021504")
	logFile := filepath.Join(logDir, fmt.Sprintf("fusion_%s.log", startTime))
	f, err := os.OpenFile(logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("打开日志文件失败: %v", err)
	}

	// 创建多输出写入器
	multiWriter := io.MultiWriter(os.Stdout, f)

	// 设置日志输出
	log.SetOutput(multiWriter)
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("Fusion v%s 启动，TOKEN: %s", Version, TOKEN)
}

func cleanOldLogs() {
	for {
		files, err := os.ReadDir(logDir)
		if err != nil {
			log.Printf("读取日志目录失败: %v", err)
			time.Sleep(24 * time.Hour)
			continue
		}

		// 按修改时间排序
		type fileInfo struct {
			name    string
			modTime time.Time
		}
		var fileList []fileInfo

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

			fileList = append(fileList, fileInfo{
				name:    name,
				modTime: fileDate,
			})
		}

		// 按时间排序
		sort.Slice(fileList, func(i, j int) bool {
			return fileList[i].modTime.After(fileList[j].modTime)
		})

		// 保留最近7天的日志，删除其他
		for i, file := range fileList {
			if i >= 7 {
				path := filepath.Join(logDir, file.name)
				if err := os.Remove(path); err != nil {
					log.Printf("删除过期日志失败 %s: %v", path, err)
				} else {
					log.Printf("已删除过期日志: %s", file.name)
				}
			}
		}

		time.Sleep(24 * time.Hour)
	}
}

func checkNodeConfig(stopChan <-chan struct{}) {
	for {
		select {
		case <-stopChan:
			log.Println("配置检查已停止")
			return
		default:
			nodePath := filepath.Join(fusionDir, "node.conf")
			info, err := os.Stat(nodePath)
			
			if err != nil {
				if os.IsNotExist(err) {
					log.Println("node.conf 不存在，执行更新")
					if err := updateNodes(); err != nil {
						log.Printf("更新节点失败: %v", err)
					}
				} else {
					log.Printf("检查 node.conf 失败: %v", err)
				}
			} else {
				// 检查文件是否超过24小时
				if time.Since(info.ModTime()) > updateInterval {
					log.Println("node.conf 超过24小时，执行更新")
					if err := updateNodes(); err != nil {
						log.Printf("更新节点失败: %v", err)
					}
				}
			}

			// 使用 select 来检查停止信号
			select {
			case <-stopChan:
				log.Println("配置检查已停止")
				return
			case <-time.After(checkInterval):
				// 继续下一次检查
			}
		}
	}
}

func main() {
	// 捕获关闭信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// 启动配置检查
	stopCheck := make(chan struct{})
	go func() {
		checkNodeConfig(stopCheck)
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
