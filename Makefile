BINARY_NAME ?= admin
GO_DIR      ?= admin
MAIN_PKG    ?= ./cmd/admin
COVERAGE_THRESHOLD ?= 85
COVERAGE_OUT = coverage.out

COMPOSE = docker compose

.PHONY: help up down logs rebuild ps test cover lint shell build run clean fmt


help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

up: ## Start the stack (docker compose up -d)
	$(COMPOSE) up -d

down: ## Stop the stack
	$(COMPOSE) down

logs: ## Follow container logs
	$(COMPOSE) logs -f

rebuild: ## Rebuild the admin image and restart it
	$(COMPOSE) build admin && $(COMPOSE) up -d admin

ps: ## Show stack status
	$(COMPOSE) ps

test: ## Run Go tests with the race detector
	cd $(GO_DIR) && go test -race ./...

cover: ## Run tests with coverage (threshold 85%)
	cd $(GO_DIR) && go test ./... -coverprofile=$(COVERAGE_OUT) -coverpkg=./...
	@TOTAL=$$(cd $(GO_DIR) && go tool cover -func=$(COVERAGE_OUT) | tail -1 | awk '{print int($$3)}'); \
	echo "Coverage: $$TOTAL% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ "$$TOTAL" -lt "$(COVERAGE_THRESHOLD)" ]; then \
		echo "FAIL: coverage $$TOTAL% is below $(COVERAGE_THRESHOLD)%"; exit 1; \
	else \
		echo "PASS: coverage $$TOTAL% >= $(COVERAGE_THRESHOLD)%"; \
	fi

lint: ## Run golangci-lint
	cd $(GO_DIR) && golangci-lint run ./...

shell: ## Test the kill switch script (and shellcheck it when installed)
	sh ovpn/killswitch_test.sh
	@if command -v shellcheck >/dev/null 2>&1; then shellcheck ovpn/*.sh; else echo "shellcheck not installed, skipped"; fi

build: ## Build the admin binary (with -trimpath)
	cd $(GO_DIR) && go build -trimpath -o ../bin/$(BINARY_NAME) $(MAIN_PKG)

fmt: ## Format Go code
	cd $(GO_DIR) && gofmt -w .

run: ## Run the admin panel locally (defaults: DB_PATH=./admin.db)
	cd $(GO_DIR) && DB_PATH=$${DB_PATH:-./admin.db} go run $(MAIN_PKG)

clean: ## Remove build artifacts
	rm -rf bin/ $(GO_DIR)/$(COVERAGE_OUT)
