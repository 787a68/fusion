# 构建阶段：使用官方 golang 镜像（alpine版）编译程序
FROM golang:1.19-alpine as builder
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN go mod download
# 构建 Go 二进制文件
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o fusion-worker .

# 运行阶段：用极简的 alpine 镜像作为基础
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]