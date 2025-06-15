FROM golang:latest AS builder

WORKDIR /app

COPY . .

# 初始化并更新依赖
RUN go mod init fusion && \
    go mod tidy && \
    go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o fusion

FROM alpine:latest

WORKDIR /app

COPY --from=builder /app/fusion .

# 创建数据目录
RUN mkdir -p /app/data

# 启动命令
CMD ["./fusion"]
