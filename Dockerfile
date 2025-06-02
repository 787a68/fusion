# 构建阶段：使用官方 golang:1.19-alpine 构建二进制文件
FROM golang:1.19-alpine AS builder
ARG VERSION
WORKDIR /app
COPY go.mod .
COPY main.go .
RUN go mod download
# 使用 ldflags 将 VERSION 注入到 main.version 变量中
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X main.version=${VERSION}" -o fusion-worker .

# 运行阶段：使用精简的 alpine 镜像
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]