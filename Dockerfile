FROM golang:1.19-alpine AS builder

ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制 go.mod 文件；如果你的仓库中没有 go.sum 文件，这里仅复制 go.mod
COPY go.mod .

# 在容器内生成或更新 go.sum 文件，下载 golang.org/x/sync/singleflight 依赖
RUN go get -d golang.org/x/sync/singleflight && go mod tidy && go mod download

# 复制源码文件
COPY main.go .

# 利用模块路径和 ldflags 编译生成可执行文件
RUN MODULE=$(go list -m -f '{{.Path}}') && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]