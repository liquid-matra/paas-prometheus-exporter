.DEFAULT_GOAL := help

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-30s\033[0m %s\n", $$1, $$2}'


.PHONY: test
test: unit ## Run tests

.PHONY: unit
unit: ## Run unit tests
	go test -v ./...

.PHONY: build
build: ## build the binary
	go build -ldflags="-X 'main.Version=v1.0.0'"

.PHONY: generate
generate: ## Generate the test mocks
	go generate ./...
