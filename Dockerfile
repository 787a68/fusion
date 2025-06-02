FROM golang:1.19-alpine AS builder

ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

# 复制全部源码（如果仓库中没有 go.mod，就不会复制）
COPY . .

# 如果没有 go.mod 文件，则自动生成一个临时的 go.mod 文件
RUN if [ ! -f go.mod ]; then \
      echo "go.mod 不存在，自动生成临时 go.mod..." && \
      go mod init github.com/temporary/module && \
      go get -d ./... ; \
    fi

# 整理依赖并下载（此时生成或更新了 go.sum 文件）
RUN go mod tidy && go mod download

# 编译前先确认模块名是否正确（本命令可以用于调试）
RUN MODULE=$(go list -m -f '{{.Path}}') && \
    echo "模块名: ${MODULE}" && \
    CGO_ENABLED=0 GOOS=linux go build -a -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]