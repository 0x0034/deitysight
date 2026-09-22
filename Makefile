VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
VERSION_LDFLAGS = -X github.com/0x0034/deitysight/internal/agent.Version=$(VERSION)

.PHONY: test race vet build verify
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o dist/deitysight-linux-amd64 ./cmd/deitysight
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "$(VERSION_LDFLAGS)" -o dist/deitysight-linux-arm64 ./cmd/deitysight
verify: test race vet build
