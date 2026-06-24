COVER_MIN   := 80
OLLAMA_HOST ?= http://localhost:11434
NPM         := npm

.PHONY: help build run playground test cover cover-firestore lint fmt typecheck arch docs-check tidy spec ollama-check ci docker

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Install the project + dependencies
	$(NPM) install

run: ## Run the agent service
	$(NPM) run start

playground: ## Launch the local ADK web UI (dev only) at :8080
	$(NPM) run playground

test: ## Run all tests
	$(NPM) test

cover: ## Run tests with coverage gate (>= $(COVER_MIN)%) over src/ (cmd is composition-only)
	$(NPM) run cover

cover-firestore: ## Run the emulator-gated firestore tests (needs a running Firestore emulator)
	@test -n "$(FIRESTORE_EMULATOR_HOST)" || { echo "set FIRESTORE_EMULATOR_HOST (start: gcloud beta emulators firestore start)"; exit 1; }
	npx vitest run src/agent/setup/parkstore_firestore.test.ts src/agent/setup/session_firestore.test.ts

lint: ## Run eslint (the go vet analogue)
	$(NPM) run lint

fmt: ## Format the code
	$(NPM) run fmt

typecheck: ## Run tsc --noEmit
	$(NPM) run typecheck

arch: ## Run architecture conformance tests
	$(NPM) run arch

docs-check: ## Verify every directory has an AGENTS.md
	$(NPM) run arch -- arch/docs.test.ts

tidy: ## Resolve/lock dependencies
	$(NPM) install --package-lock-only

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
	docker build -t automation-agent-js .

ci: lint typecheck arch test cover ## Full local CI gate
