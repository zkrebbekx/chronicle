.PHONY: test cover cover-pg fuzz bench lint fmt vet tidy clean help

test: ## Run unit tests with race detector (pgstore skips without CHRONICLE_TEST_DSN)
	go test -race -timeout 120s ./...
	cd pgstore && go test -race -timeout 300s ./...
	cd examples && go build ./...

cover: ## Run tests and open an HTML coverage report
	go test -coverprofile=coverage.out -coverpkg=./... ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "wrote coverage.html"

cover-pg: ## Coverage for the Postgres adapter (needs CHRONICLE_TEST_DSN)
	cd pgstore && go test -coverprofile=coverage.out -timeout 300s ./... && \
		go tool cover -func=coverage.out | tail -1

fuzz: ## Fuzz the write path for 30s
	go test -run=^$$ -fuzz=^FuzzPutSequence$$ -fuzztime=30s .

bench: ## Run benchmarks
	go test -run=^$$ -bench=. -benchmem ./...
	cd pgstore && go test -run=^$$ -bench=. -benchmem -benchtime=200x -timeout 600s ./...

lint: ## Run golangci-lint on both modules (must be installed)
	golangci-lint run
	cd pgstore && golangci-lint run

fmt: ## Format code
	gofmt -s -w .

vet: ## Vet every module
	go vet ./...
	cd pgstore && go vet ./...
	cd examples && go vet ./...

tidy: ## Tidy modules
	go mod tidy
	cd pgstore && go mod tidy
	cd examples && go mod tidy

clean: ## Remove build artifacts
	rm -rf bin coverage.out coverage.html pgstore/coverage.out

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
