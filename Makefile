VERSION = 0.1.1
CPA_BINARY ?= /opt/homebrew/bin/cliproxyapi
.PHONY: build test integration
build:
	mkdir -p dist
	go build -o dist/remote-compaction .
	go build -buildmode=c-shared -o dist/remote-compaction-v$(VERSION).dylib .
test:
	go test -race ./...
	go vet ./...
integration: build
	CPA_BINARY=$(CPA_BINARY) PLUGIN_LIBRARY=$(CURDIR)/dist/remote-compaction-v$(VERSION).dylib go test -v -run TestNative .
