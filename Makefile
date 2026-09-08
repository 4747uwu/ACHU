BINARY     := tarang-sender
CMD_PATH   := ./cmd/tarang-sender
VERSION    := 0.1.0
LDFLAGS    := -s -w -X main.version=$(VERSION)
BIN_DIR    := bin

# Default goal: native binary for the host OS
.PHONY: all
all: build

# ------------------------------------------------------------------------
# Build
# ------------------------------------------------------------------------

.PHONY: build
build: ## Build for the host platform
	@mkdir -p $(BIN_DIR)
	go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) $(CMD_PATH)
	@echo "→ $(BIN_DIR)/$(BINARY)"

.PHONY: build-windows
build-windows: ## Cross-compile windows/amd64 (the primary deployment target)
	@mkdir -p $(BIN_DIR)
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY).exe $(CMD_PATH)
	@echo "→ $(BIN_DIR)/$(BINARY).exe"

.PHONY: build-linux
build-linux: ## Cross-compile linux/amd64 (for testing in containers/CI)
	@mkdir -p $(BIN_DIR)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-linux $(CMD_PATH)
	@echo "→ $(BIN_DIR)/$(BINARY)-linux"

.PHONY: build-mac
build-mac: ## Cross-compile darwin/arm64 (apple silicon dev machines)
	@mkdir -p $(BIN_DIR)
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags="$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY)-darwin-arm64 $(CMD_PATH)
	@echo "→ $(BIN_DIR)/$(BINARY)-darwin-arm64"

.PHONY: build-all
build-all: build-windows build-linux build-mac

# ------------------------------------------------------------------------
# Quality gates
# ------------------------------------------------------------------------

.PHONY: test
test: ## Run all tests with race detector
	go test -race ./...

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: fmt
fmt: ## Format all Go files
	gofmt -s -w .

.PHONY: check
check: vet test ## All quality gates

# ------------------------------------------------------------------------
# Dev convenience
# ------------------------------------------------------------------------

.PHONY: run
run: ## Run with the example config (data dir under ./local-data)
	@mkdir -p local-data
	go run $(CMD_PATH) --config examples/config.example.json --data-dir ./local-data

.PHONY: tidy
tidy: ## Refresh go.mod and go.sum
	go mod tidy

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

# ------------------------------------------------------------------------
# Help
# ------------------------------------------------------------------------

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2}'
