VERSION    ?= $(shell cat VERSION)
REGISTRY   ?= ghcr.io/sagh0900
IMG        ?= $(REGISTRY)/zabbix-operator:v$(VERSION)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CONTAINER_TOOL ?= docker

PKG     := github.com/sagh0900/zabbix-operator/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).GitCommit=$(GIT_COMMIT) -X $(PKG).BuildDate=$(BUILD_DATE)

LOCALBIN ?= $(CURDIR)/bin
ENVTEST_K8S_VERSION ?= 1.30.0
GOLANGCI_LINT_VERSION ?= v1.59.1

.PHONY: help
help: ## Show targets.
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/{printf "  %-16s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

.PHONY: fmt vet lint test build
fmt: ## go fmt.
	go fmt ./...
vet: ## go vet.
	go vet ./...
lint: $(LOCALBIN) ## golangci-lint.
	test -x $(LOCALBIN)/golangci-lint || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	$(LOCALBIN)/golangci-lint run
test: fmt vet ## Unit and envtest suites.
	go test ./... -coverprofile cover.out
build: ## Build the manager binary.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/manager ./cmd/manager

.PHONY: docker-build docker-push
docker-build: ## Build the image.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMG) .
docker-push: ## Push the image.
	$(CONTAINER_TOOL) push $(IMG)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)
