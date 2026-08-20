.PHONY: deps fmt-check vet lint test test-race test-all build clean

GOLANGCI_LINT_VERSION := v2.12.2

deps:
	go mod verify

fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files are not formatted with gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

vet:
	go vet ./...

lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

test:
	go test ./...

test-race:
	go test -race ./...

test-all: deps fmt-check vet lint test-race

build:
	goreleaser release --snapshot --clean

clean:
	rm -rf -- dist
