VERSION    ?= $(shell cat VERSION)
REGISTRY   ?= ghcr.io/sagh0900
IMG        ?= $(REGISTRY)/zabbix-operator:v$(VERSION)
GIT_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
CONTAINER_TOOL ?= docker

PKG     := github.com/sagh0900/zabbix-operator/internal/version
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).GitCommit=$(GIT_COMMIT) -X $(PKG).BuildDate=$(BUILD_DATE)

LOCALBIN ?= $(CURDIR)/bin
ENVTEST_K8S_VERSION    ?= 1.37.0
GOLANGCI_LINT_VERSION  ?= v2.14.0
CONTROLLER_GEN_VERSION ?= v0.22.0
SETUP_ENVTEST_VERSION  ?= release-0.25
KUSTOMIZE_VERSION      ?= v5.8.2
PROMETHEUS_VERSION     ?= 3.15.0

CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_GEN_VERSION)
SETUP_ENVTEST  ?= $(LOCALBIN)/setup-envtest-$(SETUP_ENVTEST_VERSION)
KUSTOMIZE      ?= $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)
GOLANGCI_LINT  ?= $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
PROMTOOL       ?= $(LOCALBIN)/promtool-$(PROMETHEUS_VERSION)

.PHONY: help
help: ## Show targets.
	@awk 'BEGIN{FS=":.*##"} /^[a-zA-Z_-]+:.*##/{printf "  %-16s %s\n",$$1,$$2}' $(MAKEFILE_LIST)

.PHONY: manifests generate fmt vet lint test test-monitoring build build-installer
manifests: $(CONTROLLER_GEN) ## Generate CRDs and RBAC from markers.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases
generate: $(CONTROLLER_GEN) ## Generate deepcopy code.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
fmt: ## go fmt.
	go fmt ./...
vet: ## go vet.
	go vet ./...
lint: $(GOLANGCI_LINT) ## golangci-lint.
	$(GOLANGCI_LINT) run
test: manifests generate fmt vet $(SETUP_ENVTEST) ## Unit and envtest suites.
	@assets="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" && test -n "$$assets" || \
		{ echo "envtest assets unavailable"; exit 1; }; \
		KUBEBUILDER_ASSETS="$$assets" go test ./... -coverprofile cover.out
test-monitoring: $(PROMTOOL) ## Check and unit-test the Prometheus rules.
	@tmp=$$(mktemp -d) && trap 'rm -rf '$$tmp EXIT && \
		go run ./hack/extract-rules config/monitoring/prometheusrule.yaml > $$tmp/rules.yaml && \
		cp test/monitoring/rules_test.yaml $$tmp/ && \
		$(PROMTOOL) check rules $$tmp/rules.yaml && \
		$(PROMTOOL) test rules $$tmp/rules_test.yaml
build: ## Build the manager binary.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(LOCALBIN)/manager ./cmd/manager

build-installer: manifests $(KUSTOMIZE) ## Render dist/install.yaml, dist/monitoring.yaml and dist/dashboard.json for IMG.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image manager=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml
	cd config/manager && $(KUSTOMIZE) edit set image manager=manager:latest
	$(KUSTOMIZE) build config/monitoring > dist/monitoring.yaml
	cp config/monitoring/dashboard.json dist/dashboard.json

.PHONY: docker-build docker-push
docker-build: ## Build the image.
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --build-arg GIT_COMMIT=$(GIT_COMMIT) \
		--build-arg BUILD_DATE=$(BUILD_DATE) -t $(IMG) .
docker-push: ## Push the image.
	$(CONTAINER_TOOL) push $(IMG)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# go-install-tool installs binary $(4) from package $(2) at version $(3) as $(1).
define go-install-tool
@[ -f $(1) ] || { set -e; tmp=$$(mktemp -d); GOBIN=$$tmp go install $(2)@$(3); mv $$tmp/$(4) $(1); rm -rf $$tmp; }
endef

$(CONTROLLER_GEN): | $(LOCALBIN)
	$(call go-install-tool,$@,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VERSION),controller-gen)
$(SETUP_ENVTEST): | $(LOCALBIN)
	$(call go-install-tool,$@,sigs.k8s.io/controller-runtime/tools/setup-envtest,$(SETUP_ENVTEST_VERSION),setup-envtest)
$(KUSTOMIZE): | $(LOCALBIN)
	$(call go-install-tool,$@,sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION),kustomize)
$(PROMTOOL): | $(LOCALBIN)
	@set -e; tmp=$$(mktemp -d); \
		curl -sSfL https://github.com/prometheus/prometheus/releases/download/v$(PROMETHEUS_VERSION)/prometheus-$(PROMETHEUS_VERSION).linux-amd64.tar.gz \
		| tar -xz -C $$tmp --strip-components=1 prometheus-$(PROMETHEUS_VERSION).linux-amd64/promtool; \
		mv $$tmp/promtool $@; rm -rf $$tmp
$(GOLANGCI_LINT): | $(LOCALBIN)
	$(call go-install-tool,$@,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION),golangci-lint)
