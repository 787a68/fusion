# 构建阶段，使用 golang:1.19-alpine
FROM golang:1.19-alpine AS builder

# 定义构建参数 VERSION，并将其导出到环境变量中，便于后续命令展开
ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制模块配置和源码；如果你没有 go.sum 文件，请删除下面这行
COPY go.mod .
# COPY go.sum .

# 将源码复制进来
COPY main.go .

# 下载依赖
RUN go mod download

# 输出 VERSION 用于调试（构建日志中应能看到有效值）
RUN echo "VERSION: ${VERSION}"

# 编译二进制文件，注意这里不再使用多余的转义符，命令如下：
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X github.com/787a68/fusion.version=${VERSION}" -o fusion-worker .

# 运行阶段，使用精简的 alpine 镜像
FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]