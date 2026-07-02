.PHONY: run run-dev test build tidy fmt vet deploy help

BINARY := hello-mcp
PORT   ?= 8080

# go.mod lives at the repo root; all go commands run from there.
ROOT := $(abspath $(CURDIR)/..)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the server (auth enforced; set JWT_SIGNING_KEY)
	cd $(ROOT) && PORT=$(PORT) go run ./hello-world

run-dev: ## Run with auth bypassed (local only, no token needed)
	cd $(ROOT) && AUTH_BYPASS=true PORT=$(PORT) go run ./hello-world

build: ## Build the binary to hello-world/bin/hello-mcp
	cd $(ROOT) && go build -o hello-world/bin/$(BINARY) ./hello-world

test: ## Run the full test suite (mcpauth + hello-world)
	cd $(ROOT) && go test -race -count=1 ./...

tidy: ## Tidy go.mod/go.sum
	cd $(ROOT) && go mod tidy

fmt: ## Format the code
	cd $(ROOT) && gofmt -w .

vet: ## Run go vet
	cd $(ROOT) && go vet ./...

deploy: ## Build & deploy to Cloud Run (reads hello-world/.env)
	./scripts/deploy.sh
