BINARY     := bitcoin-shard-listener
VERSION    ?= $(shell git describe --tags --dirty 2>/dev/null || echo "dev")
LDFLAGS    := -ldflags "-X github.com/lightwebinc/bitcoin-shard-listener/metrics.Version=$(VERSION) -buildvcs=false"
BUILD_DIR  := build

.PHONY: all build test lint clean docker

all: build

build:
	mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY) .

test:
	go test ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BUILD_DIR)

docker:
	docker build -t $(BINARY):$(VERSION) .
