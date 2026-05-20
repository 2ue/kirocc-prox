export GOEXPERIMENT := jsonv2

BIN := dist/kirocc
SOURCE_SHA := $(shell { find cmd internal -type f -name '*.go' -print; printf '%s\n' go.mod go.sum Makefile .goreleaser.yaml; } 2>/dev/null | LC_ALL=C sort | xargs shasum -a 256 2>/dev/null | shasum -a 256 | cut -c1-12)
GIT_SHA ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || { test -n "$(SOURCE_SHA)" && echo source-$(SOURCE_SHA) || echo unknown; })

.PHONY: build install run debug test test-e2e lint vet fmt fix clean

build:
	go build -ldflags "-X main.gitSHA=$(GIT_SHA)" -o $(BIN) ./cmd/kirocc

install:
	go install -ldflags "-X main.gitSHA=$(GIT_SHA)" ./cmd/kirocc

run:
	go run ./cmd/kirocc $(ARGS)

debug:
	go run ./cmd/kirocc -debug $(ARGS)

test:
	go test -race ./...

test-e2e:
	go test -tags e2e -race -timeout 120s ./internal/e2e/

lint:
	golangci-lint run

vet:
	go vet ./...

fmt:
	golangci-lint fmt

fix:
	go fix ./...

clean:
	rm -f $(BIN)
