.PHONY: run run-dev test build tidy fmt vet deploy help

BINARY := omi-bridge
PORT   ?= 8080
ROOT   := $(abspath $(CURDIR)/..)

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

run: ## Run the bridge server locally
	cd $(ROOT) && PORT=$(PORT) go run ./omi-bridge

build: ## Build the binary to omi-bridge/bin/omi-bridge
	cd $(ROOT) && go build -o omi-bridge/bin/$(BINARY) ./omi-bridge

test: ## Run the full test suite
	cd $(ROOT) && go test -race -count=1 ./...

tidy: ## Tidy go.mod/go.sum
	cd $(ROOT) && go mod tidy

fmt: ## Format the code
	cd $(ROOT) && gofmt -w .

vet: ## Run go vet
	cd $(ROOT) && go vet ./...

deploy: ## Build & deploy to Cloud Run (reads omi-bridge/.env)
	./scripts/deploy.sh
