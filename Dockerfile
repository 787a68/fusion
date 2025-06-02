FROM golang:1.19-alpine AS builder

ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制 go.mod 文件
COPY go.mod .

# 如果没有提交 go.sum 文件，则在构建时生成必要的校验信息
RUN go mod tidy && go mod download

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