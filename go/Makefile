MODULE    := automation-agent
COVER_MIN := 80
OLLAMA_HOST ?= http://localhost:11434

# golangci-lint is pinned and installed from source rather than taken from PATH. A released
# binary is built with whatever Go its release used, and golangci-lint refuses to analyze a
# module whose `go` directive is newer than its own build ("the Go language version used to
# build golangci-lint is lower than the targeted Go version"). Building it with THIS module's
# toolchain — read from go.mod, so it follows a version bump automatically — sidesteps that
# entirely and makes local and CI runs identical.
GOLANGCI_VERSION := v2.12.2
GO_TOOLCHAIN     := go$(shell awk '/^go /{print $$2; exit}' go.mod)
GOBIN            := $(shell go env GOPATH)/bin
GOLANGCI         := $(GOBIN)/golangci-lint

.PHONY: help build run test cover cover-firestore lint lint-install fmt vet arch tidy tidy-check spec docs-check ollama-check ci

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Compile all packages
	go build ./...

run: ## Run the agent service
	go run ./cmd/agent

playground: ## Launch the local web UI playground (dev only) at :8080
	go run ./cmd/playground web api webui

test: ## Run all tests
	go test ./...

cover: ## Run tests with coverage gate (>= $(COVER_MIN)%) over internal/ (cmd is composition-only)
	go test -coverprofile=coverage.out -covermode=atomic ./internal/...
	@# Emulator-only backends (*_firestore.go) are validated by the FIRESTORE_EMULATOR_HOST-
	@# gated integration suite (make cover-firestore), not the default unit run, so exclude
	@# them from the gate denominator rather than require every dev to run the emulator.
	@grep -vE '_firestore\.go:' coverage.out > coverage.gate.out
	@total=$$(go tool cover -func=coverage.gate.out | awk '/total:/ {gsub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total% (excludes emulator-only *_firestore.go)"; \
	awk "BEGIN{exit !($$total >= $(COVER_MIN))}" || { echo "FAIL: coverage $$total% < $(COVER_MIN)%"; exit 1; }

cover-firestore: ## Run the firestore-backed tests against a running emulator (needs FIRESTORE_EMULATOR_HOST)
	@test -n "$$FIRESTORE_EMULATOR_HOST" || { echo "set FIRESTORE_EMULATOR_HOST first (gcloud beta emulators firestore start)"; exit 1; }
	go test ./internal/agent/setup/... -run 'Firestore|ParkStoreConformance' -count=1

lint: ## Run golangci-lint (pinned; installed on first use)
	@$(GOLANGCI) --version 2>/dev/null | grep -q '$(GOLANGCI_VERSION:v%=%)' || $(MAKE) lint-install
	@$(GOLANGCI) run

lint-install: ## (Re)install the pinned golangci-lint, built with this module's Go toolchain
	GOTOOLCHAIN=$(GO_TOOLCHAIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_VERSION)

fmt: ## Format the code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

arch: ## Run architecture conformance tests
	go test ./ARCH/...

docs-check: ## Verify the okf/ knowledge bundle conforms (frontmatter, indexes, links)
	go test ./ARCH/ -run TestOKF

tidy: ## Tidy go.mod
	go mod tidy

# go mod tidy -diff exits 1 for an untidy module *and* for a real failure (an unresolvable
# import, an unreachable proxy). The two are distinguishable by stream, not by status: the
# diff goes to stdout, a failure goes to stderr and produces no stdout. Keying on stdout keeps
# the "run make tidy" advice off failures that make tidy cannot fix, and lets the real error
# through as itself.
tidy-check: ## Verify go.mod/go.sum are already tidy, without modifying them
	@untidy=$$(go mod tidy -diff); rc=$$?; \
	if [ -n "$$untidy" ]; then \
		printf '%s\n' "$$untidy"; \
		echo "FAIL: go.mod/go.sum are not tidy — run 'make tidy'"; \
		exit 1; \
	fi; \
	exit $$rc

ollama-check: ## Verify the local Ollama server is reachable
	@curl -sf $(OLLAMA_HOST)/api/tags >/dev/null && echo "ollama up at $(OLLAMA_HOST)" || { echo "ollama not reachable at $(OLLAMA_HOST)"; exit 1; }

spec: ## Scaffold a spec from a template: make spec name=<slug> kind=<add|remove|change|migrate>
	@test -n "$(name)" || { echo "usage: make spec name=<slug> kind=<add|remove|change|migrate>"; exit 1; }
	@kind=$${kind:-change}; \
	src=../.agents/templates/$$kind.spec.md; \
	test -f $$src || { echo "no template: $$src"; exit 1; }; \
	dst=../specs/$$(date +%Y%m%d)-$(name).md; \
	cp $$src $$dst && echo "created $$dst"

docker: ## Build the production container image (cmd/agent only)
	docker build -t automation-agent .

# A gate must not change what it is measuring. `tidy` rewrites go.mod/go.sum in place, so a
# `make ci` that ran it would quietly fix an untidy module and report success — locally, and in
# CI right up until the separate `git diff --exit-code` step notices. `tidy-check` reports the
# same problem without touching the tree, so the failure names its own cause.
ci: tidy-check vet lint arch test cover ## Full local CI gate (read-only: never modifies the tree)
