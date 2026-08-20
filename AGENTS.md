# Agent instructions

## Running tests locally

Use the repository Makefile to run every local quality check before handing off
a change:

```sh
make test-all
```

This verifies dependencies and formatting, runs `go vet`, a pinned
golangci-lint version through `go run`, and the complete test suite with the
race detector. No separate linter installation is required. The individual
targets are `deps`, `fmt-check`, `vet`, `lint`, `test`, and `test-race`.

For faster iteration while developing, run the smallest relevant package first:

```sh
go test ./internal/cli
```

Replace `internal/cli` with the package being changed.

Before the complete CI workflow, these host-side checks are also useful:

```sh
gofmt -w $(git ls-files '*.go')
go mod verify
go vet ./...
make lint
go test -race ./...
```

Build snapshot binaries plus the multi-architecture executor image, and remove
generated artifacts with:

```sh
make build
make clean
```

`make build` requires GoReleaser, Docker, and Docker Buildx. It also loads the
executor variant matching the active Docker Engine under the snapshot tag used by
the CLI. Do not commit `dist/`, coverage output, or other generated artifacts.
