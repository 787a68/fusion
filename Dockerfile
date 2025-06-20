FROM golang:alpine AS builder

# 安装基本工具
RUN apk add --no-cache git gcc musl-dev

# 设置工作目录
WORKDIR /app

# 复制源代码
COPY . .

# 接收版本参数
ARG VERSION=dev

# 分步执行，便于调试
RUN go mod init fusion
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -a -o fusion \
    -ldflags "-X main.Version=${VERSION}" \
    main.go server.go update.go ingress.go egress.go

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