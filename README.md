# Fusion

Fusion 是一个用于管理和更新代理节点的工具，支持自动获取节点信息、地理位置检测、NAT 类型检测等功能。

## 功能特点

- 自动获取和更新代理节点
- 支持多订阅源
- 自动检测节点地理位置
- 自动检测 NAT 类型
- 支持自定义节点
- 自动日志管理
- Docker 支持

## 环境要求

- Go 1.16 或更高版本
- Docker（可选，用于容器化部署）
- 网络连接（用于获取节点信息）

## 安装步骤

### 从源码安装

1. 克隆仓库：
   ```bash
   git clone https://github.com/yourusername/fusion.git
   cd fusion
   ```

2. 编译：
   ```bash
   go mod tidy
   go build
   ```

### 使用 Docker

1. 构建镜像：
   ```bash
   docker build -t fusion .
   ```

2. 运行容器：
   ```bash
   docker run -d \
     -p 8080:8080 \
     -v /path/to/data/fusion:/data/fusion \
     -e SUB="your_subscription_url" \
     -e TOKEN="your_token" \
     fusion
   ```

## 配置说明

### 环境变量

- `SUB`：订阅链接，格式为 `name=url||name=url`，必填
- `TOKEN`：访问令牌，可选，未设置时自动生成
- `NODE`：自定义节点，可选，格式为多行文本

### 目录结构

- `/data/fusion`：主目录
  - `token`：存储访问令牌
  - `node.conf`：节点配置文件
  - `logs/`：日志目录

## 使用方法

1. 启动服务：
   ```bash
   ./fusion
   ```

2. 访问 API：
   ```
   http://localhost:8080?t=your_token
   ```

3. 强制更新节点：
   ```
   http://localhost:8080?t=your_token&f=1
   ```

## Docker 部署

### 构建镜像

```bash
docker build -t fusion .
```

### 运行容器

```bash
docker run -d \
  -p 8080:8080 \
  -v /path/to/data/fusion:/data/fusion \
  -e SUB="your_subscription_url" \
  -e TOKEN="your_token" \
  fusion
```

### 环境变量说明

- `SUB`：订阅链接，必填
- `TOKEN`：访问令牌，可选
- `NODE`：自定义节点，可选

## 常见问题

### 1. 节点更新失败

- 检查 `SUB` 环境变量是否正确设置
- 检查网络连接是否正常
- 查看日志文件了解详细错误信息

### 2. 无法访问 API

- 检查 `TOKEN` 是否正确
- 检查服务是否正常运行
- 检查端口是否正确开放

### 3. 日志文件过大

- 日志文件会自动按天分割
- 超过 7 天的日志会自动清理
- 可以手动删除 `logs` 目录下的旧日志文件

## 开发说明

### 项目结构

- `main.go`：主程序入口
- `server.go`：HTTP 服务器
- `update.go`：节点更新逻辑
- `ingress.go`：入站节点处理
- `egress.go`：出站节点处理

### 构建

```bash
go mod tidy
go build
```

### 测试

```bash
go test ./...
```

## 许可证

MIT License

## 贡献

欢迎提交 Issue 和 Pull Request！ 