# 使用 golang Alpine 镜像作为构建阶段
FROM golang:1.19-alpine AS builder
# 定义构建参数 VERSION
ARG VERSION

# 输出 VERSION 值用于调试（构建日志中会显示这行）
RUN echo "VERSION: ${VERSION}"

WORKDIR /app

# 复制 go.mod 与 main.go（如有 go.sum，也请复制）
COPY go.mod .
COPY go.sum .  # 如有 go.sum 文件
COPY main.go .

# 下载依赖
RUN go mod download

# 执行编译，利用 -ldflags 注入 VERSION 到 main.version 变量中
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X main.version=${VERSION}" -o fusion-worker .

# 运行阶段，使用 alpine 作为更小的基础镜像
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]