BINARY    := bitcoin-shard-listener
SINK      := sink-test-frames
VERSION   ?= $(shell git describe --tags --dirty 2>/dev/null || echo "dev")
LDFLAGS   := -ldflags "-X github.com/lightwebinc/bitcoin-shard-listener/metrics.Version=$(VERSION) -buildvcs=false"
BUILD_DIR := build

PROXY_DIR := ../bitcoin-shard-proxy
PROXY_BIN := $(PROXY_DIR)/bitcoin-shard-proxy
SEND_BIN  := $(PROXY_DIR)/send-test-frames

.PHONY: all build test test-e2e lint clean docker

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) .

$(BINARY):
	go build -buildvcs=false -o $(BINARY) .

$(SINK):
	go build -buildvcs=false -o $(SINK) ./cmd/sink-test-frames/

$(PROXY_BIN):
	$(MAKE) -C $(PROXY_DIR) bitcoin-shard-proxy

$(SEND_BIN):
	$(MAKE) -C $(PROXY_DIR) send-test-frames

test:
	go test -race ./...

test-e2e: $(BINARY) $(SINK) $(PROXY_BIN) $(SEND_BIN)
	PATH="$(CURDIR):$(abspath $(PROXY_DIR)):$$PATH" sh test/run-e2e.sh

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR) $(BINARY) $(SINK)

docker:
	docker build -t $(BINARY):$(VERSION) .
