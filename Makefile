# llm-router Makefile
#
# Basic commands:
#   make build     - build the binary into ./bin
#   make test      - run unit tests (fake upstream, no network)
#   make e2etest   - run end-to-end tests against a real upstream
#
# Extras:
#   make run       - run the router with ./config.yaml
#   make vet       - go vet
#   make fmt       - gofmt -w
#   make clean     - remove build artifacts
#   make docker    - build the Docker image

BINARY  := bin/llm-router
GO      := go
CONFIG  := config.yaml
IMG     := llm-router:latest

.PHONY: all build test e2etest run vet fmt clean docker

all: build

## build: compile the binary into ./bin
build:
	@mkdir -p bin
	$(GO) build -trimpath -o $(BINARY) .
	@echo "built $(BINARY)"

## test: run unit tests (no network required)
test:
	$(GO) test -timeout 90s ./...

## e2etest: run end-to-end tests against a real OpenAI-compatible upstream
## (skipped automatically when the upstream is not reachable)
e2etest:
	$(GO) test -v -timeout 5m ./e2e/

## run: run the router using ./config.yaml
run: build
	$(BINARY) -config $(CONFIG)

## vet: run go vet
vet:
	$(GO) vet ./...

## fmt: format the source
fmt:
	gofmt -w .

## clean: remove build artifacts
clean:
	rm -rf bin

## docker: build the Docker image
docker:
	docker build -t $(IMG) .
