.PHONY: all build test vet clean

all: build

build:
	go build ./cmd/cohotfs ./cmd/cohotfs-agent

windows-bridge:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/cohotfs-windows-bridge

test:
	go test ./...

vet:
	go vet ./...

clean:
	go clean ./...
