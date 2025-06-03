FROM golang:alpine AS builder

ARG VERSION
ENV VERSION=${VERSION}

WORKDIR /app

COPY . .

RUN go mod init github.com/temporary/module && go get -d ./...

RUN go mod tidy && go mod download

RUN MODULE=$(go list -m -f '{{.Path}}') && \
    CGO_ENABLED=0 GOOS=linux go build -ldflags "-X ${MODULE}.version=${VERSION}" -o fusion-worker .

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/fusion-worker .
EXPOSE 3000
CMD ["./fusion-worker"]
