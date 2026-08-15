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

## release: cross-compile release binaries into ./dist (used by the CI
## release workflow and the npx installer). VERSION is the release version
## without leading v (e.g. 0.1.0). Produces:
##   dist/llm-router_<VERSION>_<os>_<arch>.tar.gz  (linux/darwin x amd64/arm64)
##   dist/checksums.txt
VERSION ?= 0.1.0

release:
	@mkdir -p dist
	@for os in linux darwin; do \
	  for arch in amd64 arm64; do \
	  echo "building llm-router_$(VERSION)_$${os}_$${arch}"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath \
	    -ldflags "-s -w -X main.version=v$(VERSION)" \
	    -o dist/llm-router .; \
	  tar -czf dist/llm-router_$(VERSION)_$${os}_$${arch}.tar.gz -C dist llm-router; \
	  rm -f dist/llm-router; \
	done; done
	@cd dist && sha256sum llm-router_*.tar.gz > checksums.txt
	@ls -la dist/
