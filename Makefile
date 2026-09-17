.PHONY: test race vet build verify
test:
	go test ./...
race:
	go test -race ./...
vet:
	go vet ./...
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/deitysight-linux-amd64 ./cmd/deitysight
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o dist/deitysight-linux-arm64 ./cmd/deitysight
verify: test race vet build
