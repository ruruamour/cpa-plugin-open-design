# Makefile for the OpenDesign CPA plugin.
#
# GO can be overridden, for example:
#   make GO=/tmp/golang-go1.26.4/bin/go build

GO ?= go
VERSION ?= 0.1.0
DIST ?= dist
LIBRARY := $(DIST)/open-design.so

.PHONY: all build test test-js test-go smoke docker clean

all: test build

build:
	mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 $(GO) build \
		-trimpath -buildvcs=false -buildmode=c-shared \
		-ldflags='-s -w' -o $(LIBRARY) ./cmd/passbridge

test: test-js test-go

test-js:
	node --test tests/*.test.cjs

test-go:
	$(GO) test ./...

smoke: build
	python3 scripts/smoke-abi.py $(LIBRARY)

docker:
	docker build --platform linux/amd64 --file Dockerfile.build --output type=local,dest=$(DIST) .

clean:
	rm -rf $(DIST)
