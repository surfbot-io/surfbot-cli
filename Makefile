VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.buildDate=$(DATE)"

BIN ?= bin/surfbot-cli
PKG ?= ./cmd/surfbot-cli

.PHONY: build test lint install tidy run clean

build:
	go build $(LDFLAGS) -o $(BIN) $(PKG)

test:
	go test ./... -v -race

lint:
	golangci-lint run ./...

install:
	go install $(LDFLAGS) $(PKG)

tidy:
	go mod tidy

run: build
	$(BIN)

clean:
	rm -rf bin/ dist/
