# Designing trace-based tests

## Purpose

Traditional API tests prove that an input produced an output. A trace-based test also proves the internal distributed transaction: required services participated, dependencies behaved correctly, asynchronous side effects occurred, errors were not hidden, and critical operations met their budgets.

This makes trace-based tests especially useful for integration and end-to-end behavior that is difficult to observe from the response alone: message publication, database access, downstream calls, retries, fan-out, and third-party interactions.

## Start from a transaction, not a span

Write the observable contract in plain language first. For example:

> When checkout is accepted, the checkout service persists the order and publishes one order-created message without an error, within the agreed processing budget.

Decompose that contract into evidence:

| Contract element | Evidence |
| --- | --- |
| Request accepted | Response status and body |
| Correct service handled it | Server span with `resource["service.name"]` |
| Order persisted | Database client span or stable database operation attribute |
| Event published | Messaging producer span with stable destination or operation attributes |
| No hidden failure | Relevant spans have non-error status or expected protocol result |
| Performance acceptable | Duration of the specific critical span against a justified budget |

Do not assert every observed span. Incidental topology creates brittle tests and turns harmless refactors into failures. Assert only behavior that matters to the contract.

## Use observed evidence

Prefer creating or refining a test from a representative trace:

1. Trigger a known transaction.
2. Confirm its trace ID is the one observed by the tracing backend.
3. Inventory service names, span names, kinds, statuses, resource attributes, span attributes, and attribute types.
4. Identify which fields express stable semantics rather than current implementation detail.
5. Measure how long the complete trace and asynchronous spans take to arrive.

When no trace is available, create a conservative draft and label every assumed predicate or threshold as unverified. Do not present the test as validated.

## Choose resilient span identity

Good identity combines stable fields that distinguish the operation without coupling to a single deployment:

```hcl
matching = (
  resource["service.name"] == "checkout" &&
  span.kind == "server" &&
  span.attributes["http.request.method"] == "POST"
)
```

Use the semantic-convention version actually emitted by the target system. Older instrumentation may expose `http.method` or `http.status_code`; newer instrumentation may use names such as `http.request.method` and `http.response.status_code`. Inspect evidence instead of assuming.

Prefer, in order:

1. stable domain attributes intentionally added for the operation;
2. OpenTelemetry semantic-convention attributes;
3. service name plus span kind and a stable span name;
4. span name alone only when it is explicitly treated as a contract.

Avoid identity based on span position, generated IDs, timestamps, pod or host names, ephemeral instance IDs, or values unique to one run.

## Separate selection from behavior

Use `matching` to identify candidate spans. Use named `span_assertions` to explain what must be true of each counted span:

```hcl
spans {
  matching = (
    resource["service.name"] == "payments" &&
    span.kind == "client" &&
    span.attributes["rpc.system"] == "grpc"
  )

  span_assertions = {
    "rpc succeeded" = span.status != "error"
  }

  at_least = 1
}
```

Lamplight counts a span only when `matching` and every `span_assertions` expression are true. A failed behavioral assertion can therefore appear as a missing matching count; use clear names and inspect evidence during diagnosis.

## Choose quantity semantics deliberately

- `at_least = 1`: use for a required participant or side effect when retries or batching may create additional spans.
- `exactly = N`: use only when the exact number is an intentional invariant, such as preventing duplicate publication. Expect it to wait for complete or stable evidence.
- `at_most = N`: use for bounded retries or prohibited excess work.
- `exactly = 0`: use for absence only when the observation window and settle window are sufficient to support a negative conclusion.

An unobserved trace is not evidence of zero matching spans. It is an observability or correlation failure.

## Latency assertions

Assert duration on the operation whose budget matters, not every span in the trace:

```hcl
span_assertions = {
  "database call meets its 250ms budget" = span.duration < duration("250ms")
}
```

A latency value must come from an SLO, product requirement, or measured baseline. Include enough headroom for the intended environment. A local development test and a production-like CI environment may require different declared variables or different tests; silently loosening a threshold until it passes destroys the contract.

## Asynchronous work and negative claims

Message consumers, batch exporters, and tail sampling can delay spans after the trigger response. Measure this delay and set `observation_window` accordingly.

Positive minimum checks may pass as soon as evidence appears. Exact, maximum, and zero checks need evidence that later spans will not invalidate them, so they wait for trace completion, a stable observation, or the deadline. Tune `settle_window` from observed batching behavior rather than using it as a generic retry delay.

## Instrumentation quality

A missing expected span can mean either a product defect or an observability defect. Diagnose the layers separately:

1. Did the trigger reach the application?
2. Did the application accept and propagate `traceparent`?
3. Did downstream services retain the same trace ID?
4. Did exporters flush within the observation window?
5. Did the backend ingest and return the trace?
6. Do the normalized attribute names and types match the predicate?
7. Only then, did the expected operation actually fail to occur?

Trace-based tests also expose instrumentation weaknesses. Missing `service.name`, incorrect span kinds, unstable names, absent semantic attributes, broken parent context, and sensitive attributes should be corrected in instrumentation rather than worked around with fragile assertions.

## Review checklist

- Does the test state a business or system behavior rather than mirror a trace snapshot?
- Does it verify both the external result and the important internal effect?
- Was each predicate derived from actual normalized trace evidence?
- Are selector fields stable across deployments and harmless refactors?
- Is cardinality intentional?
- Is every latency budget justified?
- Are asynchronous and negative checks given enough measured time?
- Does a failure point to a useful layer or invariant?
- Are secrets and sensitive telemetry excluded?
- Does the test avoid unsupported relationship, event, link, or aggregate semantics?

