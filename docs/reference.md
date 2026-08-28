# Lamplight DSL and CLI reference

This document is the normative user-facing reference for the current Lamplight
DSL and CLI. The DSL has a Lamplight-specific domain model and uses HCL syntax
for blocks, expressions, and values. This reference is intentionally explicit
and repetitive enough to serve humans, editors, code generators, and LLM-based
agents.

Unless a section says otherwise:

- names and values are case-sensitive;
- unknown attributes and blocks are errors;
- relative paths are resolved according to
  [Discovery and path resolution](#discovery-and-path-resolution);
- HCL expressions use the function and variable surface documented here;
- a technical error stops the complete run;
- a failed check stops only its current test.

## Contents

1. [Project layout](#1-project-layout)
2. [Root configuration](#2-root-configuration)
3. [Tracing and metrics sources](#3-tracing-and-metrics-sources)
4. [Variables](#4-variables)
5. [Tests](#5-tests)
6. [Steps](#6-steps)
7. [HTTP requests](#7-http-requests)
   - [k6 scripts](#k6-scripts)
8. [Step outputs](#8-step-outputs)
9. [Checks](#9-checks)
10. [Expression contexts](#10-expression-contexts)
11. [Operators and functions](#11-operators-and-functions)
12. [Discovery and path resolution](#12-discovery-and-path-resolution)
13. [CLI reference](#13-cli-reference)
14. [Execution and failure semantics](#14-execution-and-failure-semantics)
15. [Trace correlation and polling](#15-trace-correlation-and-polling)
16. [Results and artifacts](#16-results-and-artifacts)
17. [Security and redaction](#17-security-and-redaction)
18. [Diagnostics](#18-diagnostics)
19. [Unsupported features](#19-unsupported-features)
20. [Agent-oriented conformance checklist](#20-agent-oriented-conformance-checklist)

## 1. Project layout

A project consists of one root configuration file and one directory containing
test definitions:

```text
project-directory/
├── .lamplight
└── lamplight/
    ├── checkout.wick
    └── health.wick
```

The root file contains exactly one `project` block, optionally one tracing
`datasource`, optionally one Prometheus `metrics` source, shared `variable`
blocks, and named `target` blocks. Files
discovered below `project.base_dir` may also contain `variable` and `test`
blocks in any directory arrangement.

The public model has five main concepts:

```text
project
├── optional datasource
├── optional metrics source
├── execution targets
└── tests
    └── ordered steps
        ├── one inline http_request
        ├── optional outputs
        └── zero or more checks
```

Variables, HTTP requests, outputs, response conditions, and span predicates are
parts of these concepts. They are not independently selectable resources.

## 2. Root configuration

### 2.1 `project` block

Exactly one `project` block is required in `.lamplight`.

```hcl
project {
  base_dir      = "./lamplight"
  output        = "pretty"
  default_target = "compose"

  http_client {
    timeout                  = duration("30s")
    follow_redirects         = true
    max_request_body_bytes   = 1048576
    max_response_body_bytes  = 10485760
    proxy                    = var.HTTP_PROXY
    tls_skip_verify          = false
  }
}
```

`default_target` is optional. Without it, `run` uses the implicit `local`
target even when other targets are declared. `--target` takes precedence over
the project default.

### 2.2 Execution targets

A target supplies a runtime and environment-specific variable values without
changing the tests:

```hcl
variable "BASE_URL" { type = string }
variable "TEMPO_ENDPOINT" { type = string }

target "compose" {
  runtime = "docker_compose"

  docker_compose {
    services = ["api", "tempo"]
  }

  variables = {
    BASE_URL       = "http://api:8080"
    TEMPO_ENDPOINT = "http://tempo:3200"
  }
}

target "cluster" {
  runtime = "kubernetes"

  kubernetes {
    context         = "production"
    namespace       = "pokeshop"
    service_account = "lamplight-runner"
  }

  variables = {
    BASE_URL       = "http://api.pokeshop.svc.cluster.local:8080"
    TEMPO_ENDPOINT = "http://tempo.monitoring.svc.cluster.local:3200"
  }
}
```

`docker_compose` inspects the running Compose project, creates a read-only
ephemeral Lamplight executor, and connects it to the networks used by the
selected services. The engine, test evaluation, polling, artifacts, and output
remain in the local CLI. Only evaluated trigger requests and datasource
operations cross the versioned stdio protocol. Lamplight does not send project
files, edit Compose files, or publish ports. `project` is an optional Compose
project name and `services` optionally restricts network discovery; by default
all running services in the current project are used.

`kubernetes` creates an attached, ephemeral executor Pod through `kubectl`. `context`,
`namespace`, and `service_account` are optional; omitted context and namespace
use the active kubectl settings. The Pod is removed after execution.

Both runtimes choose the executor image matching the CLI build automatically.
Development builds can set `LAMPLIGHT_RUNNER_IMAGE`; this is an operator escape
hatch, not project configuration.

### 2.3 Zero-code instrumentation

Lamplight can start a pinned OpenTelemetry eBPF Instrumentation (OBI) agent for
the duration of a run. Nothing is installed or configured in the application:
it does not need an OpenTelemetry SDK, exporter, OTEL environment variables,
Collector, Jaeger, or another tracing backend. The only configuration belongs
to Lamplight and identifies the ports OBI should observe:

```hcl
datasource "otlp" {
  endpoint = "http://127.0.0.1:4318"
}

instrumentation "obi" {
  open_ports          = [8080]
  context_propagation = "all"
}
```

The embedded OTLP/HTTP receiver accepts traces at `/v1/traces`, metrics at
`/v1/metrics`, and keeps both only for the current run. OBI exports its
`application` metric group every 500 milliseconds so operation checks do not
wait on its production-oriented default interval. OBI is started before the
trigger and removed after the executor exits. Lamplight uses OBI `v0.11.0` by default; `image` can pin an
explicit reviewed image:

```hcl
instrumentation "obi" {
  image      = "docker.io/otel/ebpf-instrument:v0.11.0"
  open_ports = [8080, 9090]
}
```

`open_ports` is a non-empty list of application listening ports. Supported
`context_propagation` values are `all` (default), `headers`, and `disabled`.
Use `all` when downstream calls must remain in Lamplight's authoritative W3C
trace. OBI instrumentation requires `datasource "otlp"`; it cannot be combined
with a direct-query datasource.

For a local target, OBI requires a Linux host and Docker. Lamplight runs it
with host networking, the host PID namespace, and privileged eBPF access. A
non-Linux local run fails before executing the test.

For Docker Compose, Lamplight attaches the OBI container to the application
network and exports directly to the ephemeral executor. For Kubernetes it
creates an ephemeral privileged, host-networked OBI DaemonSet and an internal
Service for the executor. The target service account must be authorized to
create and delete Pods, Services, and DaemonSets. Cluster admission policy must
permit `hostPID`, `hostNetwork`, privileged containers, and read-only host
mounts of `/sys/fs/cgroup` and `/sys/kernel/security`.

Zero-code instrumentation observes supported protocol and library boundaries;
it does not invent domain spans or domain metrics for internal business
operations. Assertions should use the signals OBI actually emits.

Target variables must refer to declared variables and match their types. Value
precedence is `--var`, `LAMPLIGHT_VAR_*`, selected target, then the variable
default. Targets cannot assign sensitive variables. Variables and expressions
are resolved locally; only values required by an evaluated trigger or
datasource operation are sent to the executor.

Properties:

| Property | Required | Type | Default | Runtime expressions | Description |
| --- | --- | --- | --- | --- | --- |
| `base_dir` | yes | literal string | — | no | Directory recursively searched for Lamplight DSL files. Resolved relative to `.lamplight`. |
| `output` | no | literal string enum | `"pretty"` | no | One of `pretty`, `text`, or `json`. |
| `default_target` | no | literal string | `local` | no | Named target used by `run` when `--target` is omitted. |

`base_dir` must exist and be a directory when the project is loaded. It cannot
reference `var` because discovery happens before runtime variable resolution.

### 2.4 `http_client` block

At most one `http_client` block is allowed inside `project`.

| Property | Required | Type | Default | Constraints |
| --- | --- | --- | --- | --- |
| `timeout` | no | `duration(...)` | `30s` | Must be positive. |
| `follow_redirects` | no | literal boolean | `true` | When false, a redirect response is returned without following it. |
| `max_request_body_bytes` | no | literal integer | `1 MiB` | Must be positive. Measured from the evaluated UTF-8 request body. |
| `max_response_body_bytes` | no | literal integer | `10 MiB` | Must be positive. The read fails after limit + 1 bytes. |
| `proxy` | no | string expression | unset | May be a literal or use `var`. Must evaluate to a valid proxy URL. |
| `tls_skip_verify` | no | literal boolean | `false` | Disables certificate verification and emits a warning when true. |

There is no per-step HTTP client configuration and no operation retry setting
in the current interface.

## 3. Tracing and metrics sources

### 3.1 Tracing datasource

At most one datasource is allowed, and its label must be `tempo`.

```hcl
datasource "tempo" {
  endpoint           = var.TEMPO_ENDPOINT
  observation_window = duration("30s")
  settle_window      = duration("2s")
  polling_interval   = duration("500ms")

  headers = {
    "X-Scope-OrgID" = var.TEMPO_TENANT
  }

  auth {
    bearer_token = var.TEMPO_TOKEN
  }

  tls {
    skip_verify = false
  }
}
```

Supported labels are `awsxray`, `azureappinsights`, `dash0`, `datadog`,
`dynatrace`, `elasticapm`, `honeycomb`, `instana`, `jaeger`, `lightstep`,
`newrelic`, `opensearch`, `otlp`, `signalfx`, `signoz`, `sumologic`, and
`tempo`.

| Adapter mode | Backends | `endpoint` meaning |
| --- | --- | --- |
| Direct query | `tempo`, `jaeger`, `elasticapm`, `opensearch`, `signalfx` | Provider query URL. Elastic/OpenSearch URLs include the index. |
| Embedded OTLP/HTTP | `otlp`, `newrelic`, `lightstep`, `datadog`, `honeycomb`, `signoz`, `dynatrace`, `instana`, `dash0` | Local listener such as `http://127.0.0.1:4318`; export traces to `/v1/traces`. The `otlp` datasource also accepts metrics at `/v1/metrics`. |
| OTLP adaptation | `awsxray`, `azureappinsights`, `sumologic` | Local listener. Lamplight ingests their collector output instead of holding cloud query credentials. |

Datasource properties:

| Property | Required | Type | Default | Expression context | Description |
| --- | --- | --- | --- | --- | --- |
| `endpoint` | yes | string expression | — | `var` | Query URL or local OTLP listener, according to the adapter table. |
| `observation_window` | no | literal `duration(...)` | `30s` | none | Hard polling limit per step. Must be positive. |
| `settle_window` | no | literal `duration(...)` | `2s` | none | Stability period used to finish negative checks early. Must be positive. |
| `polling_interval` | no | literal `duration(...)` | `500ms` | none | Delay between trace datasource observations. Must be positive. |
| `headers` | no | map of string expressions | `{}` | `var` | Headers added to readiness and trace requests. Keys are static strings. |

`auth` properties:

| Property | Required | Type | Description |
| --- | --- | --- | --- |
| `bearer_token` | yes when `auth` exists | string expression using `var` | Sent as `Authorization: Bearer <value>`. |

`tls` properties:

| Property | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `skip_verify` | no | literal boolean | `false` | Disables certificate verification for direct HTTP requests. |

Use `headers.Authorization` instead of `auth.bearer_token` when a proxy needs a
different authorization scheme, such as HTTP Basic.

The datasource is optional for response-only tests. When a selected test contains a
`spans` block, a datasource is required and `TestConnection` runs once before
the first HTTP request. `validate` and `list tests` never connect to a backend.

### 3.2 Metrics sources

At most one metrics source is allowed. A Prometheus server enables PromQL
instant queries through its stable HTTP API:

```hcl
metrics "prometheus" {
  endpoint           = var.PROMETHEUS_URL
  observation_window = duration("10s")
  settle_window      = duration("2s")
  polling_interval   = duration("500ms")

  headers = {
    "X-Scope-OrgID" = var.METRICS_TENANT
  }

  auth {
    bearer_token = var.METRICS_TOKEN
  }

  tls {
    skip_verify = false
  }
}
```

Lamplight posts each check's `query` to `/api/v1/query` immediately before and
during the observation window after the trigger. The query must return an instant vector. Aggregations such
as `sum by (...)` are therefore evaluated by Prometheus rather than recreated
inside Lamplight.

For an application exposing `/metrics` without a Prometheus server, use the
pull adapter. Scraping belongs to this project-level source, not to an
individual check:

```hcl
metrics "prometheus_scrape" {
  endpoint         = var.METRICS_URL
  polling_interval = duration("500ms")
}
```

Lamplight continuously scrapes that endpoint into a bounded in-memory time
series store. Checks run the same PromQL engine used by Prometheus over those
stored samples, including aggregations and range-vector functions.

For OBI or another OTLP Metrics producer, use Lamplight's embedded receiver.
No separate metrics source is configured: `datasource "otlp"` owns both
`/v1/traces` and `/v1/metrics`:

```hcl
datasource "otlp" {
  endpoint = "http://127.0.0.1:4318"
}
```

OTLP pushes are translated to Prometheus metric and label names and appended
to the same in-memory store. Resource attributes are available as normalized
labels prefixed with `resource_`, for example `resource_service_name`.

All three sources support `observation_window`, `settle_window`, and
`polling_interval`. The HTTP sources support `headers`, bearer authentication,
and TLS configuration. Prometheus-source defaults are `10s`, `2s`, and `500ms`;
OTLP uses its datasource defaults of `30s`, `2s`, and `500ms`.

For each step containing a metric check, Lamplight observes once immediately
before the trigger, then observes after the trigger until all checks remain
satisfied for the settle window or the observation window expires. This
before/after design verifies the metric effect of the operation rather than an
unrelated absolute value already present before the test.

## 4. Variables

Variables are declared in discovered test files and share one global namespace
across the project.

```hcl
variable "API_TOKEN" {
  type      = string
  sensitive = true
}

variable "RETRY_BUDGET" {
  type    = int
  default = 3
}

variable "EXPECTED_LATENCY" {
  type    = duration
  default = duration("500ms")
}
```

Properties:

| Property | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| block label | yes | valid HCL identifier | — | Global variable name accessed as `var.NAME`. |
| `type` | no | type keyword | `string` | Supported: `string`, `int`, `duration`. |
| `default` | no | pure expression matching the declared type | none | A variable without a default is required when selected expressions reference it. |
| `sensitive` | no | literal boolean | `false` | Marks the value for redaction. |

Variable value precedence, highest first:

1. `--var NAME=VALUE`
2. `LAMPLIGHT_VAR_<NAME>`
3. `default`

CLI and environment strings are parsed according to the declared type. An
invalid value is a technical error before any request starts.

Only variables referenced by project runtime settings, the datasource, and
selected tests are required. A missing variable used exclusively by an
unselected test does not block a run.

Sensitive values are redacted from diagnostics, result rendering, URLs,
recognized credential fields, and persisted artifacts where they are carried
as runtime values. Avoid embedding transformed secrets in unrelated strings;
perfect information-flow tracking is not possible after arbitrary expression
transformations.

## 5. Tests

```hcl
test "checkout" {
  tags = ["smoke", "checkout"]

  step "login" {
    # ...
  }

  step "create_order" {
    # ...
  }

  outputs = {
    order_id = steps.create_order.outputs.order_id
  }
}
```

Properties:

| Property | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| block label | yes | HCL string label | — | Globally unique test name used by the CLI. |
| `tags` | no | literal list or tuple of strings | `[]` | Used by repeatable `run --tag` selectors. |
| `step` | yes | one or more blocks | — | Executed in source order. Step names must be unique in the test. |
| `outputs` | no | expression map | `{}` | Reserved test-level output expressions. They are parsed and validated but are not currently emitted in `RunResult`. Prefer step outputs for active workflows. |

Tests do not call one another and are not grouped into suites. Selected tests
are sorted by name and executed sequentially.

## 6. Steps

```hcl
step "create_order" {
  http_request {
    method = "POST"
    url    = "${var.BASE_URL}/orders"
  }

  outputs = {
    order_id = response.json.order_id
  }

  check "created" {
    response = {
      "status is 201" = response.status_code == 201
    }
  }
}
```

Rules:

- The label must be a valid HCL identifier and unique in its test.
- A step contains exactly one `http_request` block.
- Steps execute in source order; there is no topological sort or parallelism.
- A step may reference outputs only from earlier steps in the same test.
- Checks always apply to the response and trace produced by their containing
  step.
- With a datasource configured, every executed step receives a distinct trace
  ID, including steps without span checks.

## 7. HTTP requests

```hcl
http_request {
  method = "POST"
  url    = "${var.BASE_URL}/orders"
  headers = {
    authorization = "Bearer ${var.API_TOKEN}"
    "content-type" = "application/json"
  }
  body = jsonencode({ customer_id = steps.login.outputs.customer_id })
}
```

Properties:

| Property | Required | Type | Expression context | Description |
| --- | --- | --- | --- | --- |
| `method` | yes | string expression | `var`, `steps` | HTTP method. Empty strings are rejected. |
| `url` | yes | string expression | `var`, `steps` | Absolute `http` or `https` URL. |
| `headers` | no | map of string expressions | `var`, `steps` | Request headers. Header names are handled case-insensitively by Go's HTTP stack. |
| `body` | no | string expression | `var`, `steps` | Request body. Use `jsonencode` for objects and collections. |

Before sending, Lamplight removes user-provided `traceparent` and `tracestate`
headers case-insensitively. If a datasource is configured, it injects the
step's generated W3C context, including `tracestate: lamplight=true`. This lets
collectors retain only Lamplight-owned traces when desired. Users cannot
replace the generated correlation context.

HTTP status codes, including 4xx and 5xx, are valid responses. Technical errors
include invalid URLs, transport failures, timeouts, body limits, unsupported
binary bodies, unsupported charsets, and malformed JSON declared as JSON.

Supported response forms:

- valid UTF-8 text;
- `text/*` content types;
- `application/json` and `*+json` content types;
- an absent content type when the body is valid UTF-8.

For JSON content types, the body must decode successfully. Binary media types
and non-UTF-8 bodies are rejected.

### k6 scripts

The executable k6 trigger runs an existing JavaScript file with the `k6`
binary available in `PATH`:

```hcl
step "checkout_load" {
  k6 {
    script = "k6/checkout.js"
    env = {
      BASE_URL = var.BASE_URL
    }
    arguments = {
      vus        = 1
      iterations = 1
      tag        = ["suite=smoke", "team=checkout"]
    }
  }

  check "k6 completed" {
    response = {
      "exit code is zero" = response.json.exit_code == 0
    }
  }
}
```

| Property | Required | Type | Expression context | Description |
| --- | --- | --- | --- | --- |
| `script` | yes | string expression | `var`, `steps` | JavaScript file below `project.base_dir`; relative paths start there. |
| `env` | no | map expression | `var`, `steps` | Environment values exposed to the script through k6 `__ENV`. |
| `arguments` | no | map | `var`, `steps` | k6 flag names and their string, number, boolean, or repeated-list values. |

Argument keys are sorted, underscores become hyphens, and Lamplight prefixes
them with `--`. String and number values become `--name=value`, `true` becomes
`--name`, `false` is omitted, and lists repeat the flag once per value. Keys
must not contain leading dashes. `summary_export` is reserved because Lamplight
manages that file.

The script must resolve to a regular file and cannot escape `project.base_dir`,
including through a symbolic link. Executable k6 triggers currently support
only the implicit `local` target. Compose and Kubernetes target execution is
rejected before starting a remote executor. Before creating temporary files or
starting the trigger, Lamplight verifies that a `k6` executable exists in
`PATH` and reports an installation-oriented diagnostic when it is absent.

When a datasource is configured, Lamplight supplies authoritative
`LAMPLIGHT_TRACEPARENT` and `LAMPLIGHT_TRACESTATE` environment variables. k6
does not apply them automatically: the script must copy them into the relevant
request headers. This keeps the k6 transaction correlated with the trace that
the containing step polls.

Lamplight runs k6 with a temporary `--summary-export` file. A successful
response has status code `0` and exposes `exit_code`, `stdout`, `stderr`, and
the decoded `summary` below `response.json`. A nonzero k6 exit, including a
failed threshold, is a trigger execution error. Output and summary sizes are
bounded by `http_client.max_response_body_bytes`.

The legacy `k6 { id = "<trace-id>" }` form remains accepted for compatibility
and attaches an already generated trace. New tests should use `traceid` for
that behavior. A k6 block must contain exactly one of `script` or `id`.

## 8. Step outputs

Step outputs are evaluated after the response arrives and before checks run.

```hcl
outputs = {
  customer_id = response.json.customer_id
  status      = response.status_code
}
```

Output names must be valid HCL identifiers. Later steps access them as:

```hcl
steps.login.outputs.customer_id
```

An unknown prior step, forward reference, unknown output, evaluation error, or
unknown value is a technical error. Outputs are data for chaining; checks do
not target an output directly and should evaluate the source response instead.

## 9. Checks

A check contains `response`, `spans`, `metrics`, or a combination.

```hcl
check "accepted and traced" {
  response = {
    "accepted" = response.status_code == 202
  }

  spans {
    matching = span.name == "enqueue"
    at_least = 1
  }
}
```

The check label is a human-facing string and does not need to be an identifier.
When multiple sections exist, all must pass.

### 9.1 Response conditions

`response` is a non-empty map of names to boolean expressions:

```hcl
response = {
  "successful status" = response.status_code >= 200 && response.status_code < 300
  "has order id"      = response.json.order_id != null
}
```

Each expression is evaluated once and reported separately. False is a normal
check failure. A type, function, traversal, or evaluation error is a technical
error. If any response condition fails, the current test stops without waiting
for spans.

### 9.2 Span checks

```hcl
spans {
  matching = (
    span.name == "POST /orders" &&
    span.attributes["http.status_code"] == 201
  )

  span_assertions = {
    "fast enough" = span.duration < duration("500ms")
  }

  exactly           = 1
  observation_window = duration("20s")
}
```

Properties:

| Property | Required | Type | Default | Description |
| --- | --- | --- | --- | --- |
| `matching` | yes | boolean expression | — | Evaluated once per observed span. |
| `span_assertions` | no | map of boolean expressions | `{}` | Assertions applied to every span selected by `matching`; every assertion must pass for every selected span. |
| `at_least` | exactly one quantity rule | non-negative literal integer | — | Minimum number of spans selected by `matching`. |
| `at_most` | exactly one quantity rule | non-negative literal integer | — | Maximum number of spans selected by `matching`. |
| `exactly` | exactly one quantity rule | non-negative literal integer | — | Exact final number of spans selected by `matching`. |
| `observation_window` | no | positive literal duration | datasource value | Per-check hard window. The step uses the largest applicable window. |

Quantity behavior:

| Rule | Early success | Early failure | Window result |
| --- | --- | --- | --- |
| `at_least = N` | as soon as at least N currently observed spans match and all their assertions pass | when any currently observed selected span fails an assertion | fails at the window if fewer than N spans were observed or an evaluated assertion failed |
| `at_most = N` | on complete or settled valid evidence within limit | when count exceeds N or any selected span fails an assertion | passes if final count is at most N and all assertions passed |
| `exactly = N` | on complete evidence with count N; `N=0` may also settle | when count exceeds N or any selected span fails an assertion | passes only if final count is N and all assertions passed |

One poller is shared by all span checks in a step. Lamplight polls immediately,
then at 500-millisecond intervals by default. A trace that never appears is not interpreted as
zero spans; it produces the technical reason `trace_not_observed`.
An assertion failure produces `span_assertion_failed` and includes aggregated
assertion evidence; a single failing selected span makes its assertion fail.

### 9.3 Metric checks

```hcl
check "order operation emitted metrics" {
  metrics {
    query = <<-PROMQL
      sum by (result) (
        orders_created_total{result="success"}
      )
    PROMQL

    metric_assertions = {
      "increments exactly once" = metric.delta == 1
    }

    exactly            = 1
    observation_window = duration("10s")
  }
}
```

`query` is a required string expression evaluated before the trigger using
`var` and prior `steps`. It is the only metric-selection mechanism for remote
Prometheus, direct scrapes, and OTLP pushes. `metric_assertions` applies to
every series returned by PromQL. Exactly one of
`at_least`, `at_most`, or `exactly` is required and counts selected time
series, not samples or increments. `observation_window` overrides the source
window upward for the containing step.

The `metric` object exposes:

| Field | Type | Meaning |
| --- | --- | --- |
| `metric.name` | string | Metric name when retained by the source; PromQL aggregations may remove `__name__`. |
| `metric.type` | string | Source type when retained; PromQL results generally leave it empty. |
| `metric.labels` | map(string) | Labels returned by PromQL, including normalized OTLP attributes. |
| `metric.attributes` | map(any) | Reserved for source evidence; PromQL results leave it empty. |
| `metric.resource` | map(any) | Reserved for source evidence; OTLP resource values are queried through `resource_*` labels. |
| `metric.previous_value` | number | Value in the pre-trigger snapshot, or zero for a new series. |
| `metric.value` | number | Value in the current post-trigger PromQL result. |
| `metric.delta` | number | `value - previous_value`; counter resets remain visible as a negative delta. |

Metric checks poll until their predicates are stable because metric export may
be asynchronous. A query, receive, scrape, or parse failure is a technical error; an incorrect
delta, value, type, or series cardinality is a normal check failure with
`metric_evidence` in JSON results. Exact deltas require an isolated target or
labels that exclude concurrent traffic. Lamplight deliberately does not infer
operation ownership from an aggregate series.

## 10. Expression contexts

Only the roots listed for a context are available.

| Location | Available roots |
| --- | --- |
| datasource endpoint, headers, bearer token | `var` |
| metrics endpoint, headers, bearer token | `var` |
| `metrics.query` | `var`, `steps` |
| `http_request` properties | `var`, `steps` |
| step `outputs` | `response`, `var`, `steps` |
| check `response` | `response`, `var`, `steps` |
| `spans.matching`, `spans.span_assertions` | `span`, `resource`, `response`, `var`, `steps` |
| `metrics.metric_assertions` | `metric`, `response`, `var`, `steps` |
| test `outputs` | `var`, `steps` |

### 10.1 `var`

```text
var.<variable-name>
```

The value has the declared `string`, `int`, or duration-as-number type.

### 10.2 `steps`

```text
steps.<prior-step-name>.outputs.<output-name>
```

Only prior steps in the same test are available.

### 10.3 `response`

| Field | Type | Description |
| --- | --- | --- |
| `response.status_code` | number | HTTP status code. |
| `response.headers` | object of string tuples | Response headers as returned by Go's HTTP client. Prefer bracket access with canonical header names. |
| `response.body` | string | Raw textual body. |
| `response.json` | dynamic value or null | Decoded JSON for JSON content types. |

Example:

```hcl
response.headers["Content-Type"][0] == "application/json"
```

### 10.4 `span`

| Field | Type | Description |
| --- | --- | --- |
| `span.trace_id` | string | Lowercase hexadecimal trace ID. |
| `span.span_id` | string | Lowercase hexadecimal span ID. |
| `span.parent_span_id` | string | Parent span ID or empty string. |
| `span.name` | string | Span name. |
| `span.kind` | string | Normalized kind such as `server` or `client`. |
| `span.status` | string | Normalized status such as `ok` or `error`. |
| `span.status_message` | string | Status description. |
| `span.duration` | number | Duration in nanoseconds; compare with `duration(...)`. |
| `span.attributes` | object/map | Normalized OTLP span attributes. |

OTLP integer, floating-point, boolean, and string attributes are exposed with
their corresponding expression types.

### 10.5 `resource`

`resource` is the normalized map of resource attributes for the candidate
span. Access dotted OpenTelemetry keys with brackets:

```hcl
resource["service.name"] == "checkout"
```

There is no additional `resource.attributes` wrapper.

## 11. Operators and functions

HCL provides normal arithmetic, equality, ordering, indexing, conditional, and
collection syntax. Common boolean operators are `&&`, `||`, and `!`.

Lamplight exposes only this pure function whitelist:

| Function | Signature | Behavior |
| --- | --- | --- |
| `lower` | `lower(string) -> string` | Unicode-aware lowercase conversion from cty stdlib. |
| `upper` | `upper(string) -> string` | Uppercase conversion. |
| `trim` | `trim(string, cutset) -> string` | Removes characters in `cutset` from both ends. |
| `trimspace` | `trimspace(string) -> string` | Removes leading and trailing whitespace. |
| `substr` | `substr(string, offset, length) -> string` | Extracts a substring. |
| `replace` | `replace(string, search, replacement) -> string` | Replaces occurrences. |
| `split` | `split(separator, string) -> list(string)` | Splits a string. |
| `join` | `join(separator, collection) -> string` | Joins string elements. |
| `contains` | `contains(collection, value) -> bool` | Tests collection membership. |
| `startswith` | `startswith(value, prefix) -> bool` | Tests a string prefix. |
| `endswith` | `endswith(value, suffix) -> bool` | Tests a string suffix. |
| `matches` | `matches(pattern, value) -> bool` | Go regular expression matched against the complete value. Add `.*` when substring matching is intended. |
| `tostring` | `tostring(value) -> string` | Converts a compatible value. |
| `tonumber` | `tonumber(value) -> number` | Converts a compatible value. |
| `tobool` | `tobool(value) -> bool` | Converts a compatible value. |
| `tolist` | `tolist(value) -> list` | Converts a compatible collection. |
| `toset` | `toset(value) -> set` | Converts a compatible collection. |
| `tomap` | `tomap(value) -> map` | Converts a compatible object or collection. |
| `duration` | `duration(string) -> number` | Parses a Go duration and returns nanoseconds. Examples: `250ms`, `2s`, `1m30s`. |
| `jsonencode` | `jsonencode(value) -> string` | Encodes a value as JSON. |
| `jsondecode` | `jsondecode(string) -> dynamic` | Decodes JSON. |

There are no filesystem, environment, network, shell, random, clock, or other
side-effecting expression functions. Environment access occurs only through
declared variables.

## 12. Discovery and path resolution

Configuration lookup:

1. `--working-dir` or `-w` changes the effective starting directory.
2. `--config` or `-c` selects a configuration file. A relative path is
   resolved from the effective working directory.
3. Without `--config`, Lamplight searches for `.lamplight` from the
   effective working directory upward to the filesystem root.
4. The real directory containing the config file becomes the project base.
5. `project.base_dir` is resolved from that config directory.

Test discovery:

- recursively visits regular `*.wick` files below `base_dir`;
- ignores other extensions, including plain `*.hcl`, so Lamplight definitions
  remain unambiguous to humans, editors, and automated agents;
- does not follow directory symlinks;
- sorts relative paths lexicographically;
- merges all variables into one global namespace;
- rejects duplicate variable or test names across files.

## 13. CLI reference

### 13.1 `lamplight init`

```text
lamplight init [-w DIR | --working-dir DIR]
```

Creates:

- `.lamplight`;
- `lamplight/`;
- `lamplight/healthcheck.wick`.

It refuses to overwrite either generated file. There is no empty-init mode.

### 13.2 `lamplight fmt`

```text
lamplight fmt [-w DIR] [FILE_OR_DIR ...]
```

Formats every `.wick` file below the working directory, or only the supplied
files and directories. The style is deterministic and non-configurable. Long
logical `&&` chains are parenthesized and split one condition per line. All
function calls longer than 50 columns are split with one argument per line.
All files are parsed before any are written, so a syntax error cannot leave a
project partially formatted.

### 13.3 `lamplight validate`

```text
lamplight validate [-c FILE] [-w DIR]
```

Performs path resolution, discovery, HCL parsing, schema validation, duplicate
checking, identifier validation, reference checking, and static expression
validation. It does not resolve required runtime values, send HTTP requests, or
connect to Tempo.

### 13.4 `lamplight list tests`

```text
lamplight list tests [-c FILE] [-w DIR]
```

Prints the test name, comma-separated tags, source file, and whether the test
contains span checks.

### 13.5 `lamplight run`

```text
lamplight run [OPTIONS] [TEST_NAME]
lamplight run [TEST_NAME] [OPTIONS]
```

Options:

| Option | Description |
| --- | --- |
| `-c FILE`, `--config FILE` | Select config file. |
| `-w DIR`, `--working-dir DIR` | Set effective working directory. |
| `--tag TAG` | Select tests containing a tag. Repeat to match any supplied tag. |
| `--file FILE` | Select tests declared in a file relative to `project.base_dir`. Repeat to match any supplied file. |
| `--exclude` | Invert an explicit test-name, file, or tag selector. |
| `--var NAME=VALUE` | Override a variable. May be repeated; duplicate names are rejected. |
| `--output FORMAT` | Override `project.output` with `pretty`, `text`, or `json`. |
| `--fail-fast` | Stop after the first failed or errored test and mark the remaining tests as skipped. |
| `--keep-artifacts` | Preserve artifacts after a successful run. |
| `--artifacts-dir DIR` | Select the parent directory for run artifacts. |

With no selector, all tests run once. Selectors by test name, file, and tag are
mutually exclusive. Repeated files or tags use OR semantics. `--exclude`
requires a selector, verifies that it matched at least one test, and then runs
the complement; a final empty selection is an error. By default, an errored or
failed test does not prevent later tests from running. `--fail-fast` stops after
the first non-passing test. A selector that matches nothing is an error.

Execution progress is written to stderr as each datasource check, test, step,
and trigger starts or completes. In an interactive terminal, in-flight triggers
and trace polling use an updating spinner. Each trace observation reports its
attempt number, total spans received, and matching span count per check. With
redirected stderr the same transitions are emitted as append-only lines. The
final selected output format is written to stdout, so JSON and text output
remain machine-readable.

### 13.6 `lamplight migrate tracetest`

```text
lamplight migrate tracetest [--output-dir DIR] [-f|--force] INPUT
```

Migrates a legacy Tracetest `type: Test` YAML file, or all `.yaml` and `.yml`
files below an input directory, into a Lamplight project. The default output
directory is the current directory. The command creates `.lamplight` when it
does not exist and writes one `lamplight/<test-name>.wick` file per test.
Flags may appear before or after `INPUT`. Every inspected file prints one status:
`processed, found N test(s)` or `ignored, no resources found`. YAML documents
that are not `type: Test` resources, including Tracetest configuration and
transactions, are ignored. A file may contain multiple YAML documents. A
compatible Tracetest `DataStore` is imported into `.lamplight`. Direct mappings
cover Jaeger, Tempo, OpenSearch, Elastic APM, and SignalFX. OTLP-based mappings
cover `otlp`, New Relic, Lightstep, Datadog, Honeycomb, SigNoz, Dynatrace,
Instana, and Dash0 using Lamplight's default local listener at
`http://127.0.0.1:4318`. Jaeger gRPC port `16685` becomes HTTP query port
`16686`; Tempo gRPC port `9095` becomes HTTP query port `3200`. Search indexes,
headers, basic authentication, and TLS skip-verification settings are retained.
The default `PollingProfile.periodic.timeout`, when present, becomes
`observation_window`.

AWS X-Ray, Azure Application Insights, and Sumo Logic provisioning is rejected:
Tracetest queries those providers directly with credentials, while Lamplight
requires a local OTLP adaptation endpoint, so there is no lossless automatic
endpoint conversion.

The migration supports HTTP and Kafka triggers, headers, bodies/messages,
`${VARIABLE}` references, single-span selectors, response status/body
assertions, span and resource attribute comparisons, simple JSON path equality
and collection membership assertions, duration assertions, and selected-span
count rules. Generated variables have no default and are supplied with
`LAMPLIGHT_VAR_<NAME>` or `--var NAME=VALUE`.

The command refuses to overwrite `.wick` files unless `--force` is supplied.
It rejects unsupported constructs inside importable tests instead of silently
discarding them. Tracetest's “assert every selected span” behavior maps directly
to `span_assertions`: the quantity rule counts every span selected by `matching`,
and every assertion must pass for every selected span.

## 14. Execution and failure semantics

Execution order:

```text
resolve project
select and sort tests
resolve required variables
connect to Tempo if selected span checks require it
for each test:
  for each step in source order:
    generate trace context when datasource exists
    evaluate request
    execute HTTP request
    evaluate step outputs
    evaluate response conditions
    poll and evaluate span checks
aggregate result
render output
finalize artifacts
exit
```

Status values are `passed`, `failed`, `error`, `cancelled`, and `skipped`.

- False response or span conditions produce `failed` and exit code 2.
- A failed check stops later steps in its test; those steps are `skipped`.
- Other selected tests continue after a check failure.
- Technical errors and cancellation stop the complete run immediately.
- Tests or steps not started after a technical stop are `skipped`.
- Cancellation and technical error both map to exit code 1.

`pending`, `no_match`, and `trace_not_observed` are polling states or reasons,
not final top-level status values.

## 15. Trace correlation and polling

When a datasource exists, the core creates a sampled W3C version 00 context
with a random 128-bit trace ID and 64-bit parent span ID for each step. This
functional context is separate from any internal observability of the
Lamplight process.

The HTTP request carries that context. Tempo is queried by the generated trace
ID. Lamplight accepts Tempo OTLP JSON responses using either `batches` or
`resourceSpans` and normalizes hexadecimal or base64-encoded OTLP IDs.

Polling lifecycle:

- immediate first query;
- one query per second afterward;
- retriable errors continue until the hard window;
- HTTP 404, 408, 429, and 5xx are retriable;
- `Retry-After` is respected when larger than the normal interval;
- invalid authentication, configuration, TLS, schema, IDs, and cancellation
  are non-retriable;
- a complete observation is authoritative;
- a partial observation may prove positive matches or an exceeded maximum but
  cannot prove absence;
- unchanged valid observations may resolve negative checks after
  `settle_window`;
- no valid observation before the hard deadline produces
  `trace_not_observed`, not a zero count.

See [Tempo integration](tempo.md) for deployment examples.

## 16. Results and artifacts

All renderers consume one `RunResult`. JSON output uses schema version 1 and is
defined by [`schemas/run-result-v1.schema.json`](../schemas/run-result-v1.schema.json).

The JSON contains:

- schema version, run ID, start time, duration, selection, status, and summary;
- tests with names, tags, source files, statuses, and steps;
- steps with execution ID, optional trace ID, redacted request/response,
  outputs, checks, diagnostics, and artifact references;
- response evidence and span count evidence;
- preserved artifact references.

Artifacts are written atomically with directories mode `0700` and files mode
`0600`. A run directory contains:

```text
run-directory/
├── metadata.json
├── step-results.json
├── checks.json
└── result.json
```

Retention:

- successful run: deleted by default;
- successful run with `--keep-artifacts`: preserved;
- failed, errored, or cancelled run: preserved;
- `--artifacts-dir`: changes the parent directory.

The artifact path is printed to stderr and included in the result when
preserved.

## 17. Security and redaction

- Declare credentials with `sensitive = true`.
- Prefer `LAMPLIGHT_VAR_NAME` over `--var` for secrets.
- Lamplight redacts known sensitive values from rendered and persisted data.
- Credential-like keys such as authorization, cookie, password, secret,
  API key, token, session, and private key are redacted recursively.
- Query parameters with sensitive names are redacted.
- Artifact files are created with restrictive permissions.
- TLS verification is enabled by default for HTTP requests and Tempo.
- User-controlled W3C propagation headers are discarded.

Redaction is a defense-in-depth feature, not a substitute for secret hygiene.
Do not write secrets into test labels, filenames, or arbitrary transformed
values.

## 18. Diagnostics

Diagnostics have severity, stable code, message, optional file and source
range, optional suggestion, and sensitivity metadata. User-facing diagnostics
go to stderr; structured run results go to stdout.

Static diagnostics include parse, schema, duplicate, identifier, reference,
path, and expression errors. Runtime diagnostics include missing variables,
request/output/check evaluation, transport, datasource, trace observation, and
artifact failures.

## 19. Unsupported features

The current language does not support:

- gRPC, Kafka, browser, shell, or custom operations;
- datasources other than Tempo;
- suites, reusable tests, actions, flows, or components;
- global or reusable checks;
- cross-test output references;
- parallel tests or steps;
- configurable HTTP operation retries;
- span relationships, events, links, or aggregate queries;
- TraceQL predicates in the DSL;
- server mode, OpenAPI API, database, cloud service, dashboard, or accounts;
- internal telemetry exporters.

Tools and agents generating HCL must not emit unsupported blocks or infer
Terraform features such as `locals`, modules, providers, or `env()`.

## 20. Agent-oriented conformance checklist

Before generating or changing a project, an automated agent should verify:

1. There is one root `project` and at most one supported `datasource` block.
2. `project.base_dir` exists and is literal.
3. Every variable and test name is globally unique.
4. Every test has at least one step.
5. Every step name and output key is a valid HCL identifier.
6. Every step has exactly one `http_request`.
7. Step references point only backward and target declared outputs.
8. Every check contains response conditions, spans, metrics, or a combination.
9. Every spans block has `matching` and exactly one quantity rule.
10. Every metrics block has `query` and exactly one quantity rule.
11. Expression roots match the context table in section 10.
12. Secrets are declared sensitive and supplied outside source control.
13. `lamplight validate` passes before `lamplight run`.
