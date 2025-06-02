# 构建阶段，使用 golang:1.19-alpine
FROM golang:1.19-alpine AS builder

# 定义构建参数 VERSION，并写入环境变量中
ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制申请文件；注意如果没有 go.sum 文件请不要复制它
COPY go.mod .
COPY main.go .

# 下载依赖
RUN go mod download

# 输出 VERSION 用于调试
RUN echo "VERSION: ${VERSION}"

# 动态获取模块路径，然后用该值构建 ldflags 参数
RUN MODULE=$(go list -m -f '{{.Path}}') && \
    echo "MODULE: ${MODULE}" && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

# 运行阶段，使用 alpine 作为更小的基础镜像
FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]