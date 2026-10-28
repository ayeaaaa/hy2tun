GO ?= go
BIN_DIR ?= bin
LDFLAGS ?= -s -w

.PHONY: all build test race bench release clean

all: build

build:
	mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/hy2tund ./cmd/hy2tund
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/hy2tun ./cmd/hy2tun

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

bench:
	$(GO) test ./... -run '^$$' -bench . -benchmem

release:
	./scripts/release.sh

clean:
	rm -rf $(BIN_DIR) dist
