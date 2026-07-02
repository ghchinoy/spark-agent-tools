.PHONY: run run-dev test build tidy fmt vet deploy token help

BINARY := hello-mcp
PORT ?= 8080

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the server (auth enforced; set JWT_SIGNING_KEY)
	PORT=$(PORT) go run .

run-dev: ## Run with auth bypassed (local only, no token needed)
	AUTH_BYPASS=true PORT=$(PORT) go run .

build: ## Build the binary to ./bin/hello-mcp
	go build -o bin/$(BINARY) .

test: ## Run the test suite
	go test ./...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

fmt: ## Format the code
	gofmt -w .

vet: ## Run go vet
	go vet ./...

deploy: ## Build & deploy to Cloud Run (reads .env)
	./scripts/deploy.sh
