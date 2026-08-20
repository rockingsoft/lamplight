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
- Queries a tracing backend by trace ID. The first implemented backend is
  Tempo.
- Preserves redacted run artifacts on failures and optionally on success.

Lamplight has no server, database, dashboard, account system, or cloud
dependency. The process starts, runs the selected tests, writes its result, and
exits.

## Status

The current MVP implements an HTTP trigger and a Tempo tracing backend. The
automated suite covers the CLI, DSL loader, expression runtime, trigger
execution, trace polling, Tempo adapter, artifacts, redaction, and renderers.
A real Tempo integration has also been validated against an external
deployment.

This is still an early project. The public DSL and JSON schema should be
treated as versioned interfaces, but users should review release notes before
upgrading once releases exist.

## Build

Requirements:

- Go 1.23 or newer.

```sh
git clone <repository-url>
cd lamplight
go build -o lamplight ./cmd/lamplight
```

Run directly without building:

```sh
go run ./cmd/lamplight --help
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

Run all tests, one test, or a tag:

```sh
./lamplight run
./lamplight run healthcheck
./lamplight run --tag smoke
```

Start the MCP server for coding agents over stdio:

```sh
./lamplight mcp --working-dir /absolute/path/to/project
```

The MCP server lets agents list and read test definitions, create or edit
`.wick` files with validation and optimistic concurrency checks, format and
lint the project, and execute selected tests. See the
[MCP server guide](docs/mcp.md) for client configuration and safety details.

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
