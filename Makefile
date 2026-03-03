.PHONY: all ci ci-go build build-cross test test-race cover lint staticcheck govulncheck actionlint shellcheck vet fmt fmt-check tidy tidy-check bootstrap-ci-tools clean docker help

BINARY  := cody
VERSION := $(shell grep 'const version' main.go | cut -d'"' -f2)
PRETTIER ?= prettier
SHFMT ?= shfmt
SHELLCHECK ?= shellcheck
GOLANGCI_LINT_TIMEOUT ?= 5m
COVERAGE_MIN ?= 80.0

all: fmt vet lint test build ## Run fmt, vet, lint, test, and build

ci-go: fmt-check tidy-check vet lint staticcheck test test-race cover build govulncheck ## Run exhaustive Go-focused CI checks

ci: ci-go actionlint shellcheck ## Run full CI validation suite (Go + workflows + shell scripts)

build: ## Build the binary
	go build -ldflags="-s -w" -o $(BINARY) .

build-cross: ## Cross-compile for common OS targets
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/$(BINARY)-linux-amd64 .
	GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/$(BINARY)-darwin-amd64 .
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -o /tmp/$(BINARY)-windows-amd64.exe .

test: ## Run all tests
	go test ./... -count=1 -shuffle=on -timeout 120s

test-race: ## Run tests with race detector
	go test -race ./... -count=1 -timeout 180s

cover: ## Run tests with coverage
	go test -tags testcoverage ./... -count=1 -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1
	@actual="$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub("%","",$$3); print $$3}')"; \
	awk -v actual="$$actual" -v min="$(COVERAGE_MIN)" 'BEGIN { \
		if (actual + 0 < min + 0) { \
			printf "Coverage gate failed: %.1f%% < %.1f%%\n", actual, min; \
			exit 1; \
		} \
		printf "Coverage gate passed: %.1f%% >= %.1f%%\n", actual, min; \
	}'
	@echo ""
	@echo "To view in browser: go tool cover -html=coverage.out"

lint: ## Run golangci-lint
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest run --timeout=$(GOLANGCI_LINT_TIMEOUT) ./...

staticcheck: ## Run staticcheck
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

govulncheck: ## Run Go vulnerability checks
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

actionlint: ## Lint GitHub Actions workflows
	go run github.com/rhysd/actionlint/cmd/actionlint@latest -color

shellcheck: ## Lint shell scripts
	@sh_files="$$(git ls-files '*.sh')"; \
	if [ -z "$$sh_files" ]; then \
		echo "No shell scripts found."; \
		exit 0; \
	fi; \
	if command -v "$(SHELLCHECK)" >/dev/null 2>&1; then \
		"$(SHELLCHECK)" -x $$sh_files; \
	elif command -v docker >/dev/null 2>&1; then \
		docker run --rm -v "$$PWD:/mnt" -w /mnt koalaman/shellcheck:stable -x $$sh_files; \
	else \
		echo "shellcheck not found (and docker unavailable). Install shellcheck to run this check locally."; \
		exit 1; \
	fi

vet: ## Run go vet
	go vet ./...

fmt: ## Format Go source files
	gofmt -w .
	@sh_files="$$(git ls-files '*.sh')"; \
	if [ -n "$$sh_files" ]; then \
		$(SHFMT) -w -i 2 -ci $$sh_files; \
	fi
	@files="$$(git ls-files '*.md' '*.yml' '*.yaml')"; \
	if [ -n "$$files" ]; then \
		prettier_cmd="$(PRETTIER)"; \
		prettier_bin="$$(printf '%s' "$$prettier_cmd" | awk '{print $$1}')"; \
		if command -v "$$prettier_bin" >/dev/null 2>&1; then \
			$$prettier_cmd --write $$files; \
		elif command -v npx >/dev/null 2>&1; then \
			npx --yes prettier --write $$files; \
		else \
			echo "Skipping Markdown/YAML formatting: prettier and npx not found in PATH."; \
		fi; \
	fi

fmt-check: ## Check Go source formatting without changing files
	@unformatted="$$(gofmt -l $$(git ls-files '*.go'))"; \
	if [ -n "$$unformatted" ]; then \
		echo "Unformatted Go files:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@sh_files="$$(git ls-files '*.sh')"; \
	if [ -n "$$sh_files" ]; then \
		$(SHFMT) -d -i 2 -ci $$sh_files; \
	fi
	@files="$$(git ls-files '*.md' '*.yml' '*.yaml')"; \
	if [ -n "$$files" ]; then \
		prettier_cmd="$(PRETTIER)"; \
		prettier_bin="$$(printf '%s' "$$prettier_cmd" | awk '{print $$1}')"; \
		if command -v "$$prettier_bin" >/dev/null 2>&1; then \
			$$prettier_cmd --check $$files; \
		elif command -v npx >/dev/null 2>&1; then \
			npx --yes prettier --check $$files; \
		else \
			echo "prettier not found in PATH and npx unavailable; install one or set PRETTIER."; \
			exit 1; \
		fi; \
	fi

tidy: ## Tidy and verify go.mod
	go mod tidy
	go mod verify

tidy-check: ## Validate go.mod/go.sum are tidy and checksums are valid
	go mod tidy -diff
	go mod verify

bootstrap-ci-tools: ## Install local CI helper tools (except shellcheck)
	go install mvdan.cc/sh/v3/cmd/shfmt@v3.10.0
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	go install honnef.co/go/tools/cmd/staticcheck@latest
	go install golang.org/x/vuln/cmd/govulncheck@latest
	go install github.com/rhysd/actionlint/cmd/actionlint@latest

clean: ## Remove build artifacts
	rm -f $(BINARY) coverage.out

docker: ## Build Docker image
	docker build -t $(BINARY):$(VERSION) -t $(BINARY):latest .

docker-up: ## Start with Docker Compose
	docker compose up -d

docker-down: ## Stop Docker Compose
	docker compose down

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-12s\033[0m %s\n", $$1, $$2}'
