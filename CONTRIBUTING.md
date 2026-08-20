# Contributing

Thank you for helping improve Lamplight. The project welcomes bug reports,
design discussion, documentation improvements, and code contributions. Please
coordinate with the maintainers before submitting substantial work so design
and compatibility decisions can be discussed early.

## Development setup

Requirements:

- Go 1.26 or newer;
- Git;
- `make`;
- GoReleaser for local release builds;
- optional access to a Tempo-compatible endpoint for manual integration tests.

Clone the repository and run every local quality check:

```sh
git clone <repository-url>
cd lamplight
make test-all
```

`make test-all` verifies dependencies and formatting, runs `go vet`, the pinned
golangci-lint version through `go run`, and the race-enabled test suite. No
separate linter installation is required. The Go tests require no database,
container, or external service.

Build a complete release snapshot and remove generated artifacts with:

```sh
make build
make clean
```

## Repository guide

Read these documents before changing a public contract:

- [`README.md`](README.md) introduces the product and common workflows.
- [`docs/reference.md`](docs/reference.md) is the normative DSL and CLI manual.
- [`docs/architecture.md`](docs/architecture.md) describes execution flow,
  package ownership, and extension patterns.
- [`docs/tempo.md`](docs/tempo.md) covers live Tempo integration.
- [`docs/provenance.md`](docs/provenance.md) records project origins and
  dependency licensing.

Go packages live under `internal`, so they are implementation details rather
than supported external Go APIs. The supported integration surfaces are the
CLI, Lamplight DSL, rendered output, JSON result, and versioned schemas.

## Quality gates

Run all of the following before opening a pull request:

```sh
make test-all
```

For focused local iteration before running the complete workflow:

```sh
gofmt -w $(git ls-files '*.go')
go mod verify
go vet ./...
make lint
go test -race ./...
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out
make build
git diff --check
```

The repository targets at least 90% statement coverage across the complete Go
test suite. Do not add tests solely to increase a number: cover success,
validation failure, runtime failure, cancellation, redaction, and relevant
boundary conditions.

Generated local binaries, coverage files, temporary artifacts, credentials,
and environment-specific test projects must not be committed.

## Change guidelines

### DSL changes

Treat the language as a public interface. A change to a block, property,
expression root, function, default, constraint, or evaluation order must:

1. be represented in the static model;
2. be rejected clearly when malformed or used in the wrong context;
3. preserve source ranges in diagnostics;
4. resolve only the variables required by selected work;
5. include loader and runtime tests;
6. update `docs/reference.md` in the same change.

Unknown blocks and attributes should remain errors. The DSL uses HCL syntax but
is not Terraform; do not silently accept Terraform constructs or implicit
environment access.

### Result and artifact changes

The JSON result is versioned. Additive or breaking changes require updates to:

- result model and aggregation;
- JSON and text renderers;
- recursive redaction;
- artifact snapshots;
- `schemas/run-result-v1.schema.json`, or a new schema version;
- reference documentation and compatibility tests.

Never create an output path that bypasses redaction. Artifact directories and
files must retain restrictive permissions.

### Tempo changes

Keep backend-specific wire formats and retry classification in
`internal/datasource/tempo`. The engine and poller operate on normalized
`model.TraceObservation` values. Adapter changes should include fixtures for
the payload shape and a manual smoke test when practical; never make the normal
test suite depend on a live service.

### Diagnostics

Prefer stable, actionable diagnostics with a short code, source location,
specific cause, and suggestion where one is useful. Never include a sensitive
runtime value in an error message.

## Documentation rules

All project documentation, user-visible examples, identifiers, and diagnostic
messages are written in English. Documentation should be usable by both humans
and automated agents:

- state whether behavior is required, optional, defaulted, or unsupported;
- enumerate accepted values and expression contexts;
- include complete copyable examples;
- distinguish validation behavior from runtime behavior;
- avoid describing planned features as implemented;
- link to the normative reference instead of creating conflicting rules.

When code and documentation disagree, fix both. The implementation is evidence
of current behavior, but undocumented behavior is not a complete public
contract.

## Pull request checklist

- [ ] The change has a focused purpose and no unrelated generated files.
- [ ] Formatting, tests, race detection, vet, build, and diff checks pass.
- [ ] New behavior has positive and negative coverage.
- [ ] Public DSL, CLI, JSON, artifact, or tracing-backend behavior is documented.
- [ ] Examples contain no real endpoints, tokens, tenant IDs, or user data.
- [ ] Secret-bearing values pass through the central redactor.
- [ ] Compatibility and migration impact are stated in the pull request.
- [ ] A live Tempo smoke test was run for relevant adapter changes, or the
      reason it was not run is recorded.

## Reporting security issues

Do not publish credentials, private trace payloads, or exploitable details in a
public issue. A private reporting channel has not yet been established; contact
the repository owner privately until `SECURITY.md` defines a formal process.

## Contribution license

The project is distributed under the [MIT License](LICENSE). Unless explicitly
stated otherwise, contributions intentionally submitted for inclusion in this
repository are provided under the same license. Contributors retain copyright
in their work.

Do not contribute code copied or adapted from another project unless its
license is compatible, its notices are preserved, and its provenance is
documented in the pull request. See [`docs/provenance.md`](docs/provenance.md).
