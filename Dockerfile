FROM golang:1.19-alpine AS builder

# 定义构建参数 VERSION 并设置为环境变量
ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制 go.mod 和源码文件（如果有 go.sum 文件，请取消注释下一行）
COPY go.mod .
# COPY go.sum .
COPY main.go .

# 下载所有依赖
RUN go mod download

# 动态获取模块路径并编译，不再输出调试信息
RUN MODULE=$(go list -m -f '{{.Path}}') && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]