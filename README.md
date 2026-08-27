# Lamplight

<p align="center">
  <img src="assets/lamplight-keeper.svg" alt="Lamplight's lighthouse keeper holding a glowing lantern" width="320">
</p>

Lamplight gives coding agents executable contracts for your application's
telemetry.

Tests are written in Wick, the Lamplight DSL. A Wick definition describes a
real workflow, the response it should produce, and the OpenTelemetry evidence
that should exist when the workflow completes. Lamplight runs the test and
verifies that evidence against your configured observability backend.

This helps agents determine whether instrumentation is correct, whether trace
context survives service boundaries, and whether a code change preserves the
runtime signals your system depends on. The same Wick files tell agents which
services, spans, attributes, outcomes, and cardinality matter when they
investigate telemetry directly in the backend.

Lamplight does not replace your observability backend. Tempo, Jaeger, Datadog,
and other platforms store and expose actual telemetry; Lamplight turns the
telemetry you expect into repeatable, agent-readable tests.

Lamplight is functional early-stage software. It runs as a single local CLI
process, with no Lamplight server, database, dashboard, account, or cloud
dependency to operate.

Lamplight is a complete rewrite of the original
[Tracetest](https://github.com/kubeshop/tracetest), led by one of its original
maintainers. See [Provenance](docs/provenance.md) for the project history and
licensing details.

## Get started

### 1. Install Lamplight

Install the latest release on macOS or Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | sh
lamplight version
```

The installer detects `amd64` or `arm64`, verifies the release checksum,
installs to `~/.local/bin`, and adds that directory to the current shell's
`PATH` when needed. The shell configuration change is idempotent.

To choose another destination or pin a release:

```sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | LAMPLIGHT_INSTALL_DIR="$HOME/bin" sh
curl -fsSL https://raw.githubusercontent.com/rockingsoft/lamplight/main/install.sh | LAMPLIGHT_VERSION=v0.1.0 sh
```

### 2. Create and run a test project

From the root of the application you want to test:

```sh
lamplight init
lamplight validate
lamplight run
```

`lamplight init` creates these files without overwriting existing ones:

```text
.
├── .lamplight
└── lamplight/
    └── healthcheck.wick
```

The generated test calls `http://localhost:8080/health`. Start your application
there, or change `BASE_URL` in `lamplight/healthcheck.wick`. You can also set it
for one run:

```sh
lamplight run --var BASE_URL=http://localhost:3000
```

The generated project configuration is intentionally small:

```hcl
project {
  base_dir = "./lamplight"
  output   = "pretty"
}
```

And the first test is a normal, editable `.wick` file:

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
        "status code" = response.status_code == 200
      }
    }
  }
}
```

Run all tests, one named test, tests from a file, or tests containing any of
several tags. Add `--exclude` to invert any explicit selector:

```sh
lamplight run
lamplight run healthcheck
lamplight run --tag smoke
lamplight run --tag slow --tag flaky --exclude
lamplight run --file checkout/orders.wick --exclude
lamplight run --fail-fast
```

To run trace assertions without instrumenting or reconfiguring the application,
use Lamplight's embedded OTLP receiver and OBI zero-code instrumentation. There
is no SDK to install, no OTEL environment variables to inject, and no Collector,
Jaeger, or other tracing backend to deploy with the application:

```hcl
datasource "otlp" {
  endpoint = "http://127.0.0.1:4318"
}

instrumentation "obi" {
  open_ports = [8080]
}
```

The blocks describe the Lamplight test environment and the application ports;
they do not change the application. During each run Lamplight creates the OBI
agent and in-memory receiver, then removes the agent when the run finishes.
This mode supports Linux local runs, Docker Compose targets, and Kubernetes
targets. It uses elevated eBPF privileges and is intentionally unsupported for
local macOS and Windows processes. See the [configuration reference](docs/reference.md#23-zero-code-instrumentation)
for runtime requirements and Kubernetes permissions.

Lamplight prints live trigger and trace-polling progress. A run continues after
a failed test by default so it can report the complete result; use
`--fail-fast` when an early exit is more useful.

### 3. Connect Lamplight to a coding agent with MCP

Lamplight includes a local stdio MCP server. Add it from your application root
so the server is bound to that project's `.lamplight` file.

For Codex:

```sh
codex mcp add lamplight -- lamplight mcp --working-dir "$PWD"
```

For Claude Code, store the configuration with the project:

```sh
claude mcp add --scope project lamplight -- lamplight mcp --working-dir "$PWD"
```

Check the connection with the client you configured:

```sh
codex mcp list
claude mcp list
```

The MCP server lets an agent discover the exact DSL and supported triggers,
scaffold and validate tests without writing, inspect redacted trace evidence,
list and read definitions and targets, safely edit `.wick` files and the active
`.lamplight` configuration, format and lint projects, and execute selected
tests. Existing writes use content hashes, validation, atomic rename, and
rollback to avoid silently overwriting concurrent or invalid changes. See the
[MCP server guide](docs/mcp.md) for its tools, project scoping, targets, and
secret-handling guidance.

Once connected, try prompts such as:

> Read the Wick tests and tell me which telemetry proves that checkout
> completed successfully.

> Run the smoke tests and use the failed telemetry contract to identify which
> layer I should investigate.

> Compare this trace with the Wick contract and check whether instrumentation
> still emits the required spans and attributes.

> Add a regression test that ensures the order event is published exactly
> once.

You can also install the repository's agent skills. They teach an agent how to
design resilient trace-based tests and how to instrument an application with
OpenTelemetry:

```sh
npx skills add rockingsoft/lamplight \
  --skill lamplight-trace-tests opentelemetry-instrumentation \
  --agent claude-code codex
```

Add `--global` to make the skills available across projects.

## Wick as an executable telemetry contract

A Wick test documents the runtime evidence that proves a workflow is behaving
correctly. Every step executes a trigger, exposes its result to checks and
later steps, and creates an independent W3C trace context when trace assertions
are used.

For example, this test verifies both an API response and the corresponding
service operation:

```hcl
variable "BASE_URL" {
  type = string
}

variable "API_TOKEN" {
  type      = string
  sensitive = true
}

test "create_order" {
  tags = ["orders", "smoke"]

  step "request" {
    http_request {
      method = "POST"
      url    = "${var.BASE_URL}/orders"
      headers = {
        authorization  = "Bearer ${var.API_TOKEN}"
        "content-type" = "application/json"
      }
      body = jsonencode({ customer_id = "customer-123" })
    }

    check "order accepted and traced" {
      response = {
        "created" = response.status_code == 201
      }

      spans {
        matching = (
          resource["service.name"] == "orders" &&
          span.name == "create order" &&
          span.attributes["order.status"] == "created"
        )
        exactly = 1
      }
    }
  }
}
```

From this definition, an agent can learn that:

- the workflow begins with `POST /orders`;
- the `orders` service must participate;
- `create order` is the operation that proves processing occurred;
- `order.status = created` is the relevant outcome;
- the operation must happen exactly once.

Wick is the source of expected telemetry. The observability backend is the
source of actual telemetry. When an agent can access both, it can use the
contract to guide trace searches, compare observed signals with expected
behavior, and decide whether a failure belongs to the application,
instrumentation, export pipeline, backend, or test definition.

Supply secrets through environment variables so they do not remain in shell
history or agent transcripts:

```sh
LAMPLIGHT_VAR_API_TOKEN="$API_TOKEN" \
  lamplight run create_order --var BASE_URL=http://localhost:8080
```

The application under test must honor Lamplight's incoming `traceparent` and
export downstream spans with the same trace ID. A tracing backend cannot
correlate a workflow if the application breaks context propagation.

Lamplight supports ordered HTTP and backend triggers, including executable
local k6 scripts, direct trace-by-ID integrations, and local OTLP ingestion
adapters, as well as local, Docker Compose, and Kubernetes execution targets.
Executable k6 triggers require `k6` in `PATH`. See the
[DSL and CLI reference](docs/reference.md) for the current trigger, datasource,
target, expression, and configuration contracts.

## The agent feedback loop

```text
Agent reads the code and Wick contracts
                  ↓
Agent identifies the expected behavior and telemetry
                  ↓
Lamplight triggers the workflow
                  ↓
The application exports OpenTelemetry data
                  ↓
The observability backend exposes actual telemetry
                  ↓
Lamplight verifies the contract
                  ↓
Agent diagnoses, changes, and verifies the code
```

Lamplight does not replace unit tests, logs, or observability platforms. It
makes telemetry expectations explicit, executable, and useful to coding agents.

## Everyday commands

Validate, inspect, and format a project:

```sh
lamplight validate
lamplight list tests
lamplight fmt
```

Choose machine-readable output for CI or another tool:

```sh
lamplight run --output text
lamplight run --output json
```

Output formats are `pretty` for interactive use, deterministic `text` without
ANSI escapes, and versioned `json` described by
[`schemas/run-result-v1.schema.json`](schemas/run-result-v1.schema.json).

Exit codes are stable:

| Code | Meaning |
| --- | --- |
| `0` | Every selected test and check passed. |
| `1` | A technical error or cancellation occurred. |
| `2` | At least one check failed and no technical error occurred. |

Migrate compatible Tracetest YAML projects:

```sh
lamplight migrate tracetest ./tracetest-tests --output-dir ./migrated
lamplight validate --working-dir ./migrated
lamplight fmt --working-dir ./migrated
```

The migrator handles compatible triggers and datasource provisioning and
rejects constructs that cannot be represented safely instead of silently
changing their meaning.

## Documentation

- [DSL and CLI reference](docs/reference.md) — the normative user-facing
  contract for files, blocks, expressions, execution, outputs, and errors.
- [MCP server](docs/mcp.md) — agent tools, configuration, and write safety.
- [Architecture](docs/architecture.md) — runtime flow, package ownership, and
  extension points.
- [Tempo integration](docs/tempo.md) — configuration, propagation, polling,
  authentication, and troubleshooting.
- [Provenance](docs/provenance.md) — project lineage and dependency licenses.

## Contributing

Contributions and bug reports are welcome. To work on Lamplight itself:

```sh
git clone https://github.com/rockingsoft/lamplight.git
cd lamplight
make test-all
```

`make test-all` verifies dependencies and formatting, runs `go vet` and the
pinned linter, and executes the complete test suite with the race detector. Go
1.26 or newer and `make` are required; no external service is needed for the
test suite.

Read [CONTRIBUTING.md](CONTRIBUTING.md) for the development workflow, public
contract rules, coverage expectations, release builds, and pull request
checklist.

## License

Lamplight is open-source software licensed under the [MIT License](LICENSE).
