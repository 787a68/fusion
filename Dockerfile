FROM golang:alpine AS builder

# 安装基本工具
RUN apk add --no-cache git gcc musl-dev

# 设置工作目录
WORKDIR /app

# 兼容无 go.sum，适配最新 Docker 版本
COPY go.mod go.sum* ./

# 利用 buildx 缓存加速 go mod download
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod download || go mod tidy

# 再复制全部源码
COPY . .

# 接收版本参数
ARG VERSION=dev

# 编译时继续挂载缓存
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags "-X main.Version=${VERSION}" -o fusion .

# 最终镜像
FROM alpine:latest

# 安装必要的系统工具
RUN apk add --no-cache ca-certificates tzdata bind-tools traceroute curl iputils

# 创建工作目录和日志目录
RUN mkdir -p /data/fusion/logs

# 从builder复制编译好的程序
COPY --from=builder /app/fusion /usr/local/bin/

# 设置环境变量
ENV TZ=Asia/Shanghai

# 暴露端口
EXPOSE 8080

# 设置持久化卷
VOLUME ["/data/fusion"]

# 运行程序
CMD ["fusion"]
