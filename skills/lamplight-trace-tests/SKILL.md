---
name: lamplight-trace-tests
description: Design and author trace-based integration and end-to-end tests with Lamplight. Use when turning distributed-system behavior, OpenTelemetry traces, service interactions, asynchronous side effects, or latency expectations into resilient Lamplight .wick tests. Do not use for generic unit tests or observability dashboards.
license: MIT
---

# Lamplight trace-based tests

Create tests that validate an entire distributed transaction, not merely the trigger response. Treat trace data as behavioral evidence: which services and dependencies participated, whether expected side effects occurred, whether spans succeeded, and whether critical operations stayed within an intentional latency budget.

Execution requires a Lamplight project and access to its configured tracing backend. Design and static validation do not require backend access.

## Design the observable contract

Before writing HCL:

1. Identify the user or system outcome and the trigger that initiates it.
2. Describe the expected transaction path and externally meaningful side effects.
3. Obtain a representative trace when possible. Inspect actual span names, kinds, statuses, attribute names, value types, resource attributes, and ingestion delay.
4. Separate response invariants from trace invariants.
5. Select spans by stable semantic identity, then assert their expected behavior.

Read [references/design-guide.md](references/design-guide.md) when deciding what to test, reviewing an existing trace-based test, or diagnosing flakiness.

Read [references/lamplight-authoring.md](references/lamplight-authoring.md) before creating or editing `.lamplight` or `.wick` files. It contains the supported DSL shape, expression surface, quantity and polling semantics, and complete examples.

## Authoring workflow

1. Inspect the target repository's `.lamplight`, existing `.wick` files, and Lamplight version or bundled reference. Preserve local conventions.
2. Confirm the application propagates Lamplight's W3C trace context and exports the correlated spans. A tracing backend cannot repair broken propagation.
3. Write the smallest test that proves the observable contract:
   - trigger the behavior with realistic but non-sensitive input;
   - gate on the response when one exists;
   - add named span checks for important internal outcomes and side effects;
   - use explicit variables for environment-specific values and secrets.
4. Run `lamplight validate` before any execution.
5. Run one selected test with JSON output and retained artifacts when execution is authorized:

   ```sh
   lamplight run <test-name> --output json --keep-artifacts
   ```

6. Compare predicates with normalized observed evidence. Do not guess attribute names or coerce numeric and boolean attributes into strings.
7. Tighten the test only after the broad predicate matches consistently.

If Lamplight is exposed through MCP, prefer its read, edit, format, lint, validate, and selected-run operations over hand-editing when those operations are available. Do not start services, alter instrumentation, or execute a live test unless the user requested or authorized those effects.

## Required quality bar

- Name checks after behavior and evidence, not implementation tickets.
- Prefer OpenTelemetry semantic conventions and stable domain attributes over incidental span names.
- Use `resource["service.name"]`, `span.kind`, and a stable operation or domain attribute together when one field alone is ambiguous.
- Keep span identity in `matching`; keep behavioral expectations in named `span_assertions`.
- Prefer `at_least` for required participation. Use `exactly` only when cardinality is itself a contract, and `at_most` or zero only for meaningful negative guarantees.
- Base latency limits on an explicit SLO, requirement, or measured baseline with justified headroom. Never invent a threshold.
- Account for asynchronous export and side effects with measured observation windows. Do not increase timeouts blindly.
- Avoid assertions on generated trace IDs, span IDs, timestamps, random values, deployment-specific instance IDs, or unstable ordering.
- Never put credentials or customer data in source, labels, examples, or diagnostics. Declare secret variables as sensitive and supply them through the environment.
- Do not emit syntax unsupported by the installed Lamplight version. In particular, do not translate Tracetest selector syntax, parent-child selectors, TraceQL, span events, links, or aggregate queries into Lamplight unless the target version documents them.

## Completion

Report separately:

- the behavioral contract encoded by the test;
- the observed trace evidence used to choose predicates;
- validation results;
- execution results, if execution was authorized;
- anything not verified, including unavailable traces, unmeasured latency budgets, or unsupported relationship assertions.
