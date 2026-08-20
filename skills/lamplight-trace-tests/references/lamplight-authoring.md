# Lamplight authoring reference

Use the target repository's current Lamplight documentation as authoritative when it differs from this reference.

## Project shape

```text
project/
├── .lamplight
└── lamplight/
    ├── variables.wick
    └── checkout.wick
```

Minimal root configuration with a direct Tempo-compatible backend:

```hcl
project {
  base_dir = "./lamplight"
  output   = "pretty"
}

datasource "tempo" {
  endpoint           = var.TEMPO_ENDPOINT
  observation_window = duration("30s")
  settle_window      = duration("2s")

  auth {
    bearer_token = var.TEMPO_TOKEN
  }
}
```

Declare datasource variables in a discovered file such as
`lamplight/variables.wick`:

```hcl
variable "TEMPO_ENDPOINT" {
  type = string
}

variable "TEMPO_TOKEN" {
  type      = string
  sensitive = true
}
```

The installed Lamplight version may support additional datasource labels and trigger blocks. Inspect its bundled reference before using them. Do not infer Terraform providers, modules, locals, or `env()`.

## Complete trace-based test

```hcl
variable "BASE_URL" {
  type = string
}

variable "API_TOKEN" {
  type      = string
  sensitive = true
}

variable "MAX_PERSIST_DURATION" {
  type    = duration
  default = duration("500ms")
}

test "create_order" {
  tags = ["integration", "orders", "tracing"]

  step "create" {
    http_request {
      method = "POST"
      url    = "${var.BASE_URL}/orders"
      headers = {
        authorization  = "Bearer ${var.API_TOKEN}"
        "content-type" = "application/json"
      }
      body = jsonencode({ customer_id = "test-customer" })
    }

    check "order accepted" {
      response = {
        "status is 201" = response.status_code == 201
        "order id returned" = response.json.order_id != null
      }
    }

    check "order persisted" {
      spans {
        matching = (
          resource["service.name"] == "orders" &&
          span.kind == "client" &&
          span.attributes["db.system"] == "postgresql"
        )

        span_assertions = {
          "database span succeeded" = span.status != "error"
          "persistence meets budget" = span.duration < var.MAX_PERSIST_DURATION
        }

        at_least = 1
      }
    }

    check "order event published once" {
      spans {
        matching = (
          resource["service.name"] == "orders" &&
          span.kind == "producer" &&
          span.attributes["messaging.destination.name"] == "orders.created"
        )

        span_assertions = {
          "publish succeeded" = span.status != "error"
        }

        exactly            = 1
        observation_window = duration("20s")
      }
    }
  }
}
```

The attribute names in this example are illustrative. Replace them with the exact normalized attributes and types emitted by the target instrumentation.

## Check semantics

A check contains `response`, `spans`, or both. When both exist, both must pass. If a response condition fails, Lamplight stops that test before polling spans for the step.

Every `spans` block requires:

- one boolean `matching` expression;
- zero or more named boolean `span_assertions`;
- exactly one of `at_least`, `at_most`, or `exactly`;
- optionally, a positive `observation_window`.

Available span fields include:

- `span.trace_id`, `span.span_id`, and `span.parent_span_id`;
- `span.name`, `span.kind`, `span.status`, and `span.status_message`;
- `span.duration`, compared with `duration(...)`;
- `span.attributes`, preserving normalized scalar types;
- `resource`, the resource-attribute map, accessed as `resource["service.name"]`.

Span predicates can also reference `response`, declared `var` values, and prior `steps` outputs. Do not use `resource.attributes`; there is no wrapper.

## Polling semantics

- Lamplight injects an authoritative W3C trace context into each correlated trigger.
- All span checks for a step share one trace-observation lifecycle.
- `at_least` can succeed as soon as its threshold is reached.
- `at_most` fails as soon as its limit is exceeded.
- `exactly` fails as soon as its count is exceeded; positive exact counts otherwise wait for complete or settled evidence or the deadline.
- A trace never observed produces `trace_not_observed`; it does not satisfy a zero-count assertion.
- Partial traces can prove positive matches or an exceeded maximum, but cannot prove absence.

## Validation and diagnosis

Validate without network calls:

```sh
lamplight validate
```

Execute one selected test and retain normalized evidence:

```sh
lamplight run create_order --output json --keep-artifacts
```

If a predicate does not match:

1. Inspect the reported trace ID and retained artifacts.
2. Confirm that the same trace ID exists in the backend.
3. Start with `matching = span.name != ""`.
4. Add service, kind, operation, and attribute conditions one at a time.
5. Match the exact normalized attribute types; `201` is not `"201"`.
6. Remember that all `span_assertions` must also pass for the span to count.

Increase observation windows only after measuring ingestion or asynchronous-processing delay. Fix propagation, exporter, backend, or predicate problems at their actual layer.
