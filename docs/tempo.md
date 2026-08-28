# Tempo integration

Lamplight can query a Grafana Tempo-compatible trace-by-ID API after executing
an HTTP step. Tempo is optional for response-only tests and required when a
selected test contains a `spans` block.

This guide covers deployment concerns. The complete datasource and span
language contract is in the [DSL and CLI reference](reference.md).

## Prerequisites

You need:

- an HTTP or HTTPS endpoint that exposes Tempo's readiness and trace-by-ID
  routes;
- any required authentication and tenant headers;
- an application under test instrumented with OpenTelemetry or another
  compatible tracer;
- trace propagation from the incoming HTTP request to exported spans.

Lamplight injects a W3C `traceparent` header into each step request. The
application must accept and propagate that context. If it starts an unrelated
trace, Lamplight cannot correlate the resulting spans even when Tempo is
healthy and receiving data.

## Direct Tempo configuration

For an endpoint where Tempo is directly reachable:

```hcl
variable "TEMPO_ENDPOINT" {
  type    = string
  default = "http://localhost:3200"
}

datasource "tempo" {
  endpoint           = var.TEMPO_ENDPOINT
  observation_window = duration("30s")
  settle_window      = duration("2s")
  polling_interval   = duration("500ms")
}
```

Lamplight resolves these paths below the configured base endpoint:

```text
GET /ready
GET /api/traces/<32-character-lowercase-hex-trace-id>
```

An endpoint may include a path prefix. Paths are joined safely, so
`https://example.test/tempo` becomes `https://example.test/tempo/ready` and
`https://example.test/tempo/api/traces/<id>`.

## Authentication and tenancy

Bearer authentication and a tenant header can be configured independently:

```hcl
variable "TEMPO_TOKEN" {
  type      = string
  sensitive = true
}

variable "TEMPO_TENANT" {
  type = string
}

datasource "tempo" {
  endpoint = "https://tempo.example.test"

  headers = {
    "X-Scope-OrgID" = var.TEMPO_TENANT
  }

  auth {
    bearer_token = var.TEMPO_TOKEN
  }
}
```

Supply credentials outside source control:

```sh
export LAMPLIGHT_VAR_TEMPO_TOKEN='replace-me'
export LAMPLIGHT_VAR_TEMPO_TENANT='team-a'
lamplight run traced_healthcheck
```

For Basic authentication or a proxy-specific scheme, set the complete
`Authorization` value in `headers` instead of using `auth`:

```hcl
variable "TEMPO_AUTHORIZATION" {
  type      = string
  sensitive = true
}

datasource "tempo" {
  endpoint = "https://observability.example.test/tempo"
  headers = {
    Authorization   = var.TEMPO_AUTHORIZATION
    "X-Scope-OrgID" = "team-a"
  }
}
```

Do not define both forms unless the bearer token is intended to replace the
header value.

## Grafana datasource proxy

Grafana can proxy datasource requests, but its route must expose both readiness
and trace-by-ID paths in the shape expected above. A typical base URL resembles:

```text
https://grafana.example.test/api/datasources/proxy/uid/<tempo-datasource-uid>
```

Whether `/ready` is available through that proxy depends on Grafana and
datasource configuration. Verify both endpoints before using the proxy. If the
readiness route is unavailable, use a direct Tempo or reverse-proxy endpoint;
the current client does not support a separate readiness URL.

Example:

```hcl
variable "GRAFANA_AUTHORIZATION" {
  type      = string
  sensitive = true
}

datasource "tempo" {
  endpoint = "https://grafana.example.test/api/datasources/proxy/uid/tempo"
  headers = {
    Authorization = var.GRAFANA_AUTHORIZATION
  }
}
```

## Defining a span check

```hcl
test "traced_healthcheck" {
  tags = ["smoke", "tracing"]

  step "request" {
    http_request {
      method = "GET"
      url    = "${var.BASE_URL}/health"
    }

    check "response and server span" {
      response = {
        "status is 200" = response.status_code == 200
      }

      spans {
        matching = (
          span.kind == "server" &&
          resource["service.name"] == "example-api"
        )
        at_least = 1
      }
    }
  }
}
```

`matching` and every `assertions` expression are applied to each span. A
span contributes to the count only when all predicates return true. Resource
attributes are accessed directly through `resource`, not through
`resource.attributes`.

## Polling behavior

Before the first step of a run, Lamplight calls `/ready` once if any selected
test needs trace data. It then performs the HTTP request and polls the step's
trace ID immediately and once per configured `polling_interval`.

- `observation_window` is the hard deadline. The default is 30 seconds.
- `polling_interval` is the delay between trace queries. The default is 500
  milliseconds.
- `settle_window` is the stability period used for negative checks. The
  default is 2 seconds.
- A per-check `observation_window` can extend the effective step window. The
  longest applicable window wins.
- All span checks in a step share the same observations.
- `at_least` passes as soon as its threshold is reached.
- `at_most` fails as soon as its limit is exceeded. It passes on explicit trace
  completion, a stable observation, or a satisfied deadline observation.
- `exactly` fails as soon as its count is exceeded. Positive exact counts wait
  for completion or the deadline; `exactly = 0` may pass after stability.

HTTP `404`, `408`, `429`, and `5xx` responses from trace lookup are retried
within the observation window. A positive numeric `Retry-After` header is
honored. Most transport failures are retriable; invalid DNS names, certificate
errors, cancellation, malformed payloads, and mismatched trace IDs are not.

If no valid trace is observed before the deadline, the run reports a technical
`trace_not_observed` error rather than a failed assertion. A partial trace at
the deadline produces a failed check with `partial_observation` evidence.

## Accepted response shape

The adapter accepts Tempo/OTLP JSON with top-level `batches` or
`resourceSpans`. It reads `scopeSpans` and the older
`instrumentationLibrarySpans` name. Trace, span, and parent IDs may be lowercase
or uppercase hexadecimal or standard base64 and are normalized to lowercase
hexadecimal.

Span kind and status accept common OTLP string or numeric representations.
OTLP attribute wrappers such as `stringValue`, `intValue`, `doubleValue`, and
`boolValue` are normalized into HCL-compatible scalar values. Response bodies
larger than 32 MiB are not fully consumed.

## TLS

Certificate verification is enabled by default. For a temporary development
environment only:

```hcl
datasource "tempo" {
  endpoint = "https://tempo.local"

  tls {
    skip_verify = true
  }
}
```

Prefer installing the correct CA certificate. Do not commit
`skip_verify = true` as a production workaround.

## Verification workflow

Use this sequence when validating a deployment:

1. Confirm the application accepts W3C trace context and exports spans.
2. Confirm the configured base endpoint serves `/ready` with a 2xx response.
3. Run `lamplight validate` to check HCL without making network calls.
4. Run one response-only test to isolate application connectivity.
5. Run one traced test with `--output json --keep-artifacts`.
6. Inspect the reported step trace ID and confirm the same ID is queryable from
   Tempo.
7. Verify resource and span attribute names against the normalized JSON rather
   than assuming semantic-convention versions.

Example commands:

```sh
lamplight validate
lamplight run traced_healthcheck --output json --keep-artifacts
```

## Troubleshooting

### Datasource readiness fails

- Check that the configured endpoint is a base URL, not the full trace path.
- Request `<endpoint>/ready` with the same headers from the same machine.
- Verify tenant and authorization headers.
- Check DNS, certificate trust, reverse-proxy path rewriting, and network
  policy.

### HTTP response passes but the trace is not observed

- Confirm the service reads the incoming `traceparent` header.
- Confirm downstream services propagate the same trace ID.
- Confirm exporters are enabled and flush within the observation window.
- Compare the step's reported `trace_id` with Tempo directly.
- Increase `observation_window` only after measuring ingestion delay.

### A span predicate never matches

- Keep artifacts and inspect normalized span evidence.
- Use the exact attribute types: numeric values are not strings.
- Access resource attributes as `resource["key"]`.
- Remember that `matching` and all `assertions` must pass on the same span.
- Start with a broad predicate such as `span.name != ""`, then add conditions
  one at a time.

### Negative checks pass or fail later than expected

Negative claims require evidence that no later spans will invalidate them.
Tempo may not provide explicit completion, so Lamplight waits for a stable
fingerprint over `settle_window` or for the hard deadline. Increase the settle
window when traces routinely arrive in delayed batches.
