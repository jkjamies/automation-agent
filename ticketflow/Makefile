.PHONY: build test cover vet fmt check run

build: ## Compile the binary into ./bin
	go build -o bin/ticketflow ./cmd/ticketflow

test: ## Run every test (needs git and sh on PATH; no network, no model)
	go test ./...

cover: ## Tests with a per-package coverage report
	go test -cover ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt
	gofmt -w .

check: vet test ## What CI should run
	@test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt needed"; exit 1; }

run: ## Dry-run one ticket: make run KEY=PROJ-123
	go run ./cmd/ticketflow run -dry-run $(KEY)
