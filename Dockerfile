FROM golang:1.19-alpine AS builder

ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制 go.mod 文件
COPY go.mod .

# 显式获取依赖，再自动生成 go.sum 条目，并下载其它依赖
RUN go get golang.org/x/sync/singleflight && go mod tidy && go mod download

# 复制源码文件
COPY main.go .

# 动态获取模块路径并编译
RUN MODULE=$(go list -m -f '{{.Path}}') && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]