# 构建阶段：使用 golang:1.19-alpine 作为构建环境
FROM golang:1.19-alpine AS builder
ARG VERSION

# 输出传入的 VERSION 以便调试
RUN echo "VERSION: ${VERSION}"

WORKDIR /app

# 复制 go.mod 和 main.go（如果没有 go.sum，不要复制）
COPY go.mod .
COPY main.go .

# 下载所有依赖
RUN go mod download

# 编译二进制文件，并利用 ldflags 注入版本号
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X main.version=${VERSION}" -o fusion-worker .

# 运行阶段：使用 alpine 镜像
FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]