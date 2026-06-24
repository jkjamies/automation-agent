COVER_MIN  := 80
OLLAMA_HOST ?= http://localhost:11434
UV         := uv

.PHONY: help build run playground test cover cover-firestore lint fmt typecheck arch docs-check tidy spec ollama-check ci docker

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ## Install the project + dependencies into the virtualenv
	$(UV) sync

run: ## Run the agent service
	$(UV) run python cmd/agent/main.py

playground: ## Launch the local ADK web UI (dev only) at :8080
	$(UV) run adk web --port 8080 cmd/playground

test: ## Run all tests
	$(UV) run pytest -q

cover: ## Run tests with coverage gate (>= $(COVER_MIN)%) over automation_agent/ (cmd is composition-only)
	$(UV) run pytest --cov=automation_agent --cov-report=term-missing --cov-fail-under=$(COVER_MIN) -q

cover-firestore: ## Run the firestore-backed tests against a running emulator (needs FIRESTORE_EMULATOR_HOST)
	@test -n "$$FIRESTORE_EMULATOR_HOST" || { echo "set FIRESTORE_EMULATOR_HOST first (gcloud emulators firestore start)"; exit 1; }
	$(UV) run pytest tests/test_setup_parkstore.py tests/test_setup_session.py -q

lint: ## Run ruff
	$(UV) run ruff check .

fmt: ## Format the code
	$(UV) run ruff format .

typecheck: ## Run mypy (the go vet analogue)
	$(UV) run mypy automation_agent cmd

arch: ## Run architecture conformance tests
	$(UV) run pytest arch -q

docs-check: ## Verify every directory has an AGENTS.md
	$(UV) run pytest arch/test_docs.py -q

tidy: ## Resolve/lock dependencies
	$(UV) lock

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
	docker build -t automation-agent-py .

ci: lint typecheck arch test cover ## Full local CI gate
