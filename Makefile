# azure-gateway-api

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILT   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
IMAGE   ?= azure-gateway-api

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.built=$(BUILT)

.PHONY: help
help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the binary for this machine
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/gateway ./cmd/gateway

.PHONY: test
test: ## Run the full test suite with the race detector
	go test -race ./...

.PHONY: test-short
test-short: ## Run tests without the race detector
	go test ./...

.PHONY: cover
cover: ## Report test coverage
	go test -coverprofile=coverage.out -coverpkg=./internal/... ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: lint
lint: ## Vet, format check and staticcheck if present
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	@command -v staticcheck >/dev/null 2>&1 && staticcheck ./... || echo "staticcheck not installed, skipped"

.PHONY: check
check: lint test ## Everything CI would run

.PHONY: image
image: ## Build the container image for linux/amd64
	docker build --platform linux/amd64 \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILT=$(BUILT) \
	  -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

.PHONY: probe
probe: build ## Interrogate the configured containers (needs DI_UPSTREAM_URL and READ_UPSTREAM_URL)
	./bin/gateway probe

.PHONY: run
run: build ## Run locally against upstreams from the environment
	DATA_DIR=$${DATA_DIR:-./data} ./bin/gateway serve

.PHONY: sdk-test
sdk-test: ## Prove the gateway against the real Azure SDKs (see test/sdk/README.md)
	cd test/sdk && ./run.sh

.PHONY: clean
clean: ## Remove build output and local data
	rm -rf bin dist data coverage.out
