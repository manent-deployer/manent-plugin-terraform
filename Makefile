# Build, test and lint of the external plugin.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build test lint

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/manent-plugin-terraform .

test:
	go test ./...

lint:
	gofmt -l . | (! grep .) && go vet ./...
