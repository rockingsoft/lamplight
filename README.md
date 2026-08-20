# Lamplight

<p align="center">
  <img src="assets/lamplight-keeper.svg" alt="Lamplight's lighthouse keeper holding a glowing lantern" width="320">
</p>

Lamplight is a declarative test runner for distributed systems. Tests use the
Lamplight DSL to define triggers, pass data between steps, and assert behavior
using trigger results and observability data. The DSL has its own domain model
and uses HCL as its concrete syntax and expression engine.

This project is a new-generation fork and reimplementation of the original
[Tracetest](https://github.com/kubeshop/tracetest), led by one of its original
maintainers. It preserves the goal of testing distributed systems through
observability while exploring a smaller, declarative architecture. See
[Provenance](docs/provenance.md).

## What it does

- Reads project and test definitions written in the Lamplight DSL.
- Executes ordered triggers. The first implemented trigger is `http_request`.
- Passes outputs from one step to later steps.
- Correlates each trigger execution with an independent W3C trace context when
  a tracing backend is configured.
- Checks trigger results and trace data with named DSL expressions.
- Queries tracing backends by trace ID through direct provider APIs or receives
  traces through an embedded OTLP/HTTP endpoint.
- Preserves redacted run artifacts on failures and optionally on success.

Lamplight has no server, database, dashboard, account system, or cloud
dependency. The process starts, runs the selected tests, writes its result, and
exits.

Tests can also run inside a target network without exposing its tracing or
application ports. Named targets support an automatically managed ephemeral
container in an existing Docker Compose project or an ephemeral Kubernetes Pod;
both execute the same Lamplight binary and require no Lamplight server.

## Status

The current MVP implements an HTTP trigger and all tracing backend families
supported by the original Tracetest: Tempo, Jaeger, Elastic APM, OpenSearch,
SignalFx/Splunk Observability, OTLP, New Relic, Lightstep, Datadog, Honeycomb,
SigNoz, Dynatrace, Instana, Dash0, AWS X-Ray, Azure Application Insights, and
Sumo Logic. The
automated suite covers the CLI, DSL loader, expression runtime, trigger
execution, trace polling, backend adapters, OTLP ingestion, artifacts,
redaction, and renderers.
A real Tempo integration has also been validated against an external
deployment.

This is still an early project. The public DSL and JSON schema should be
treated as versioned interfaces, but users should review release notes before
upgrading once releases exist.

## Install

Install the latest Lamplight release on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | sh
```

The installer detects `amd64`/`arm64`, verifies the release checksum, and uses
`/usr/local/bin` when writable or `~/.local/bin` otherwise. Override the
destination or pin a version when needed:

```sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | LAMPLIGHT_INSTALL_DIR="$HOME/bin" sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | LAMPLIGHT_VERSION=v0.1.0 sh
```

Confirm the installed version with `lamplight version`.

## Build

Requirements:

- Go 1.26 or newer.
- `make` for the repository development commands.

```sh
git clone <repository-url>
cd lamplight
go build -o lamplight ./cmd/lamplight
```

Run directly without building:

```sh
go run ./cmd/lamplight --help
```

### Local checks

Run the standard Go test suite or every local quality check:

```sh
make test
make test-all
```

`make test-all` verifies modules and formatting, runs `go vet`, golangci-lint,
and the complete test suite with the race detector. The lint target runs its
pinned golangci-lint version through `go run`, so no separate installation is
required. Individual targets are available as `deps`, `fmt-check`, `vet`,
`lint`, `test`, and `test-race`.

### Multi-platform builds

[GoReleaser](https://goreleaser.com/) builds release archives for macOS,
Linux, and Windows on both `amd64` and `arm64`. Validate the configuration and
create a complete local snapshot without publishing it:

```sh
goreleaser check
make build
```

Generated binaries, archives, and checksums are written to `dist/`. GoReleaser
also builds the executor image for `linux/amd64` and `linux/arm64`. Snapshot
images use platform-suffixed tags; set `LAMPLIGHT_RUNNER_IMAGE` when running a
snapshot binary against one of them locally.

Remove all generated GoReleaser artifacts with:

```sh
make clean
```

`make build` requires GoReleaser, Docker, and Docker Buildx. Release builds
publish the same `linux/amd64` and `linux/arm64` executor image as a registry
manifest under the CLI version.

CI performs a snapshot build on pull requests and pushes to `main`. Pushing a
semantic-version tag publishes the corresponding GitHub release:

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The current CLI prints its command usage when invoked without a command.

## Quick start

Create a project:

```sh
./lamplight init
```

This creates `.lamplight` and `lamplight/healthcheck.wick` without
overwriting existing files.

Validate and inspect the project:

```sh
./lamplight validate
./lamplight list tests
```

Migrate legacy Tracetest YAML tests into a new Lamplight project:

```sh
./lamplight migrate tracetest ./tracetest-tests --output-dir ./migrated
./lamplight validate --working-dir ./migrated
./lamplight fmt --working-dir ./migrated
```

The migrator supports Tracetest HTTP and Kafka tests plus compatible datastore
provisioning, reports one styled processed or ignored status per input file,
and fails explicitly on constructs that cannot be represented safely. See the
CLI reference for the conversion contract.

Format every `.wick` test below the current directory with Lamplight's fixed,
canonical style (including readable wrapping for long `&&` chains):

```sh
./lamplight fmt
```

Run all tests, one test, or a tag:

```sh
./lamplight run
./lamplight run healthcheck
./lamplight run --tag smoke
./lamplight run --fail-fast
```

Runs continue with later tests after a failure or technical error by default.
Use `--fail-fast` to stop at the first non-passing test. Live progress is
printed while tests are running, including trigger results and per-attempt span
match counts during trace polling.

Start the MCP server for coding agents over stdio:

```sh
./lamplight mcp --working-dir /absolute/path/to/project
```

The MCP server lets agents list and read test definitions and targets, create
or edit `.wick` files and the active `.lamplight` configuration with validation
and optimistic concurrency checks, format and lint the project, and execute
selected tests against an optional named target. See the
[MCP server guide](docs/mcp.md) for client configuration and safety details.

### Agent skills

The repository includes complementary Agent Skills-compatible guides:

- `lamplight-trace-tests` teaches agents to design resilient trace-based tests,
  choose meaningful span evidence, and author Lamplight definitions;
- `opentelemetry-instrumentation` teaches agents to instrument applications
  with automatic instrumentation first, configure exporters and environment
  profiles, and add useful non-duplicative manual spans.

From a local checkout, install both for Claude Code and Codex with:

```sh
npx skills add . \
  --skill lamplight-trace-tests opentelemetry-instrumentation \
  --agent claude-code codex
```

After publishing the repository on GitHub, install them remotely with:

```sh
npx skills add <owner>/lamplight \
  --skill lamplight-trace-tests opentelemetry-instrumentation \
  --agent claude-code codex
```

Add `--global` to make the skills available across projects. Their source
lives under [`skills`](skills).

Supply runtime variables through the environment or CLI:

```sh
LAMPLIGHT_VAR_API_TOKEN="$API_TOKEN" ./lamplight run checkout
./lamplight run checkout --var BASE_URL=http://localhost:8080
```

Environment variables are preferred for secrets. Passing a sensitive variable
with `--var` produces a warning because shell history and process listings may
retain it.

## Minimal project

`.lamplight`:

```hcl
project {
  base_dir = "./lamplight"
  output   = "pretty"
}
```

`lamplight/healthcheck.wick`:

```hcl
variable "BASE_URL" {
  type    = string
  default = "http://localhost:8080"
}

test "healthcheck" {
  tags = ["smoke"]

  step "health" {
    http_request {
      method = "GET"
      url    = "${var.BASE_URL}/health"
    }

    check "healthy" {
      response = {
        "status is 200" = response.status_code == 200
      }
    }
  }
}
```

## HTTP and trace example

```hcl
test "create_order" {
  step "request" {
    http_request {
      method = "POST"
      url    = "${var.BASE_URL}/orders"
      headers = {
        authorization = "Bearer ${var.API_TOKEN}"
        "content-type" = "application/json"
      }
      body = jsonencode({ customer_id = var.CUSTOMER_ID })
    }

    check "order accepted and traced" {
      response = {
        "created" = response.status_code == 201
      }

      spans {
        matching = (
          span.name == "POST /orders" &&
          span.attributes["http.status_code"] == 201
        )
        at_least = 1
      }
    }
  }
}
```

The application under test must honor the incoming `traceparent` header and
export spans that retain its trace ID. Configuring Tempo alone cannot correlate
traces from an application that ignores trace propagation.

## Output and exit codes

Choose output in the project or override it for one run:

```sh
./lamplight run --output pretty
./lamplight run --output text
./lamplight run --output json
```

- `pretty` is intended for interactive use.
- `text` is deterministic and contains no ANSI escape sequences.
- `json` follows [`schemas/run-result-v1.schema.json`](schemas/run-result-v1.schema.json).

Exit codes:

| Code | Meaning |
| --- | --- |
| `0` | Every selected test and check passed. |
| `1` | A technical error or cancellation occurred. |
| `2` | At least one check failed and no technical error occurred. |

## Documentation

- [DSL and CLI reference](docs/reference.md) — complete user-facing
  contract for files, blocks, properties, expressions, functions, execution,
  outputs, and errors.
- [Architecture](docs/architecture.md) — runtime flow, package organization,
  interfaces, state machines, and extension points.
- [MCP server](docs/mcp.md) — agent tools, configuration, and write safety.
- [Tempo integration](docs/tempo.md) — configuration, propagation,
  authentication, polling, and troubleshooting.
- [Contributing](CONTRIBUTING.md) — development workflow, quality gates, and
  change rules.
- [Provenance](docs/provenance.md) — relationship to the original project and
  dependency licensing notes.

For humans and coding agents, `docs/reference.md` is the normative usage
manual. Examples elsewhere are introductory and must not override that
reference.

## Current scope

The MVP intentionally excludes gRPC, Kafka, browser automation, non-Tempo
datasources, suites, reusable actions, parallel execution, operation retries,
server APIs, persistence databases, dashboards, and cloud services.

## License

Lamplight is open-source software licensed under the [MIT License](LICENSE).
Dependency and project-lineage details are documented in
[Provenance](docs/provenance.md).
