# Everything you need to work on ullage. `make help` lists it.
#
# The only hard requirement is Go. Every target below runs without a
# Kubernetes cluster, without a GPU, and without a cloud account, except the
# ones under "end to end" which say so.

SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
BIN     := bin/ullage
IMAGE   ?= ghcr.io/ullage-project/ullage:$(VERSION)

##@ Getting started

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

.PHONY: demo
demo: build ## See what ullage does, with no cluster and no GPU
	$(BIN) demo

.PHONY: tour
tour: build ## A narrated two-minute walkthrough of the whole idea
	@ULLAGE=$(BIN) ./examples/tour.sh

##@ Build

.PHONY: build
build: ## Build ./bin/ullage with the version stamped in
	@mkdir -p bin
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/ullage
	@echo "built $(BIN) — $$($(BIN) version)"

.PHONY: install
install: ## Install ullage into $$GOBIN
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/ullage

.PHONY: image
image: ## Build the container image locally
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .
	@echo "built $(IMAGE)"

.PHONY: clean
clean: ## Remove build output
	rm -rf bin dist

##@ Test

.PHONY: test
test: ## Run the unit tests with the race detector
	go test -race ./...

.PHONY: cover
cover: ## Run tests and open the coverage report
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

.PHONY: lint
lint: fmt vet ## Everything CI checks, minus the tests
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./... ; \
	else \
		echo "golangci-lint not installed, skipping."; \
		echo "  brew install golangci-lint   (or see https://golangci-lint.run)"; \
	fi

.PHONY: fmt
fmt: ## Check formatting
	@out="$$(gofmt -l .)"; \
	if [ -n "$$out" ]; then echo "not gofmt-clean:"; echo "$$out"; exit 1; fi
	@echo "gofmt clean"

.PHONY: vet
vet: ## Run go vet
	go vet ./...

.PHONY: vuln
vuln: ## Check dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# The checks that only ever fail when the tool is run the way a user runs it.
# Both of the bugs this catches shipped past a green unit suite.
.PHONY: smoke
smoke: build ## Run the checks that only fail when you actually type the commands
	@echo "==> the documented durations parse"
	@for d in 14d 1w 24h 1d12h 90m; do \
		$(BIN) demo --window "$$d" --quiet --exit-zero >/dev/null \
			|| { echo "--window $$d failed to parse"; exit 1; }; \
	done
	@echo "==> invalid input is refused"
	@if $(BIN) demo --window bogus --exit-zero >/dev/null 2>&1; then \
		echo "--window bogus was accepted"; exit 1; fi
	@echo "==> the JSON contract holds"
	@$(BIN) demo --output json --quiet --exit-zero | python3 -c 'import json,sys; \
r=json.load(sys.stdin); s=r["scan"]; \
excl=sum(e["accelerators"] for e in r["notAnalyzed"]); \
assert s["acceleratorsAnalyzed"]+excl==s["acceleratorsObserved"], "census does not reconcile"; \
assert r["recommendations"], "no findings"; \
print("  contract ok, %d findings" % len(r["recommendations"]))'
	@echo "==> the example scripts run"
	@for f in examples/*.sh e2e/kind.sh; do bash -n "$$f" || exit 1; done
	@if command -v shellcheck >/dev/null; then \
		shellcheck -S warning examples/*.sh e2e/*.sh \
			|| { echo "shellcheck found problems"; exit 1; }; \
	fi
	@PAUSE=0 NO_COLOR=1 ULLAGE=$(BIN) ./examples/tour.sh >/dev/null \
		|| { echo "examples/tour.sh failed"; exit 1; }
	@if command -v jq >/dev/null; then \
		ULLAGE=$(BIN) ./examples/weekly-digest.sh >/dev/null \
			|| { echo "examples/weekly-digest.sh failed"; exit 1; }; \
		ULLAGE=$(BIN) BUDGET_USD=999999 ./examples/ci-gate.sh >/dev/null \
			|| { echo "examples/ci-gate.sh should pass under a huge budget"; exit 1; }; \
		ULLAGE=$(BIN) BUDGET_USD=1 ./examples/ci-gate.sh >/dev/null 2>&1 \
			&& { echo "examples/ci-gate.sh should fail under a \$$1 budget"; exit 1; }; \
		echo "  examples ok"; \
	else echo "  jq absent, skipped the two JSON examples"; fi
	@echo "all smoke checks passed"

.PHONY: check
check: lint test smoke ## Everything CI runs. Run this before you open a PR.
	@echo "ready to push"

##@ End to end (needs a cluster)

.PHONY: e2e-kind
e2e-kind: ## Stand up a local kind cluster with fake GPUs and scan it
	./e2e/kind.sh up

.PHONY: e2e-kind-down
e2e-kind-down: ## Tear the local kind cluster down
	./e2e/kind.sh down
