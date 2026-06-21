MODULE    := github.com/jkjamies/automation-agent
COVER_MIN := 80
OLLAMA_HOST ?= http://localhost:11434

.PHONY: help build run test cover lint fmt vet arch tidy spec docs-check ollama-check ci

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
	@total=$$(go tool cover -func=coverage.out | awk '/total:/ {gsub("%","",$$3); print $$3}'); \
	echo "total coverage: $$total%"; \
	awk "BEGIN{exit !($$total >= $(COVER_MIN))}" || { echo "FAIL: coverage $$total% < $(COVER_MIN)%"; exit 1; }

lint: ## Run golangci-lint
	golangci-lint run

fmt: ## Format the code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

arch: ## Run architecture conformance tests
	go test ./ARCH/...

docs-check: ## Verify every directory has an AGENTS.md
	go test ./ARCH/ -run TestEveryDirHasAgentsDoc

tidy: ## Tidy go.mod
	go mod tidy

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

ci: tidy vet arch test cover ## Full local CI gate
