.PHONY: test test-race build

test:
	go test ./...

test-race:
	go test -race ./...

build:
	mkdir -p build
	go build -buildmode=c-shared -o build/cliproxy-cursor-acp.so ./cmd/cliproxy-cursor-acp
