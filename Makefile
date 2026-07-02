.PHONY: test build tidy fmt vet lint help

# ── Monorepo root Makefile ────────────────────────────────────────────────────
# Runs quality gates across the whole module (pkg/mcpauth + hello-world).
# To build or run a specific tool, cd into its directory and use its Makefile:
#
#   cd hello-world && make run-dev
#   cd hello-world && make deploy
#
# The Dockerfile at the repo root is used by Cloud Run source deploy.
# For local Docker builds: docker build -t hello-mcp .

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

test: ## Run the full test suite (all packages)
	go test -race -count=1 ./...

build: ## Build all tools
	go build ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

fmt: ## Format all Go source
	gofmt -w .

vet: ## Run go vet across all packages
	go vet ./...

lint: ## Run golangci-lint (must report 0 issues)
	golangci-lint run
