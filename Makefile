BINARY := wecom-push
PREFIX ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/Cinea4678/wecom-push/internal/version.Version=$(VERSION) \
	-X github.com/Cinea4678/wecom-push/internal/version.Commit=$(COMMIT) \
	-X github.com/Cinea4678/wecom-push/internal/version.Date=$(DATE)

.PHONY: build test vet clean install

build:
	mkdir -p bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/wecom-push

test:
	go test -race -v ./...

vet:
	go vet ./...

install: build
	mkdir -p $(PREFIX)
	install -m 0755 bin/$(BINARY) $(PREFIX)/$(BINARY)

clean:
	rm -rf bin dist
