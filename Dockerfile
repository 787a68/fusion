FROM golang:latest AS builder

WORKDIR /app

COPY . .

# 初始化并更新依赖
RUN go mod init fusion && \
    go mod tidy && \
    go mod download

# 显示详细的构建错误
RUN CGO_ENABLED=0 GOOS=linux go build -v -x -o fusion

FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/fusion .

# 创建数据目录
RUN mkdir -p /app/data

# 启动命令
CMD ["./fusion"]
