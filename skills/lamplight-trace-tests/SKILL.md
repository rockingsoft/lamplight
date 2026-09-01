---
name: lamplight-trace-tests
description: Design, author, and interpret Lamplight Wick contracts for trace-based integration and end-to-end tests. Use when turning distributed-system behavior or expected OpenTelemetry evidence into resilient .wick tests, or when using existing Wick definitions to guide telemetry investigation. Do not use for generic unit tests or unconstrained observability exploration.
license: MIT
---

# Lamplight trace-based tests

Create tests that validate an entire distributed transaction, not merely the trigger response. Treat each Wick definition as an executable contract for expected telemetry: which services and dependencies should participate, which evidence proves a side effect occurred, how many times an operation may happen, and whether critical operations stay within an intentional latency budget.

Execution requires a Lamplight project and access to its configured tracing backend. Design and static validation do not require backend access.

Lamplight is not a general telemetry explorer. Wick is the source of expected telemetry; the configured observability backend is the source of actual telemetry. When backend query tools are available, use Wick predicates to guide focused searches and compare observed evidence with the contract. Do not imply that Lamplight provides arbitrary backend exploration.

## Design the observable contract

Before writing or changing Wick:

1. Identify the user or system outcome and the trigger that initiates it.
2. Describe the expected transaction path and externally meaningful side effects.
3. Obtain a representative trace when possible. Inspect actual span names, kinds, statuses, attribute names, value types, resource attributes, and ingestion delay.
4. Separate response invariants from trace invariants.
5. Select spans by stable semantic identity, then assert their expected behavior.

Read [references/design-guide.md](references/design-guide.md) when deciding what to test, reviewing an existing trace-based test, or diagnosing flakiness.

Read [references/lamplight-authoring.md](references/lamplight-authoring.md) before creating or editing `.lamplight` or `.wick` files. It contains the supported DSL shape, expression surface, quantity and polling semantics, and complete examples.

## Authoring workflow

1. Inspect the target repository's `.lamplight`, existing `.wick` files, and Lamplight version or bundled reference. Read existing Wicks as the project's declared telemetry contracts before inspecting the backend or proposing new predicates.
2. Confirm the application propagates Lamplight's W3C trace context and exports the correlated spans. A tracing backend cannot repair broken propagation.
3. Write the smallest test that proves the observable contract:
   - trigger the behavior with realistic but non-sensitive input;
   - gate on the response when one exists;
   - add named span checks for important internal outcomes and side effects;
   - use explicit variables for environment-specific values and secrets.
4. Run `lamplight validate` before any execution.
5. Run one selected test with JSON output and retained artifacts when execution is authorized:

   ```sh
   lamplight run <test-name> --json-file result.json --keep-artifacts
   ```

6. Compare predicates with normalized observed evidence. Do not guess attribute names or coerce numeric and boolean attributes into strings.
7. Tighten the test only after the broad predicate matches consistently.

## Use Wick to investigate telemetry

When diagnosing telemetry or a failed test:

1. Read the selected Wick and extract the expected trigger, participating services, span identity, attributes and types, behavioral assertions, cardinality, and observation window.
2. Run the test only when execution is authorized. Capture its trace ID and normalized evidence.
3. If a backend query tool is available, search actual telemetry by that trace ID first. Use the Wick fields to focus the investigation rather than starting with an unconstrained query.
4. Compare expected and actual telemetry by layer: trigger execution, context propagation, instrumentation, exporter or Collector delivery, backend ingestion, normalization, and application behavior.
5. Treat a mismatch as evidence to diagnose, not automatic proof that the application is wrong. The Wick may be stale, the instrumentation may be incomplete, or the trace may be partial.

If no backend access is available, report what the Wick says should exist and what Lamplight observed. Do not claim to have inspected raw backend telemetry.

If Lamplight is exposed through MCP, discover its current capabilities first. Prefer its scoped reference, scaffold, prospective-content validation, read, edit, format, lint, trace-observation, and selected-run operations over hand-editing when those operations are available. Treat trace observation as a live backend read and test execution as an external effect; do not start services, alter instrumentation, observe a live backend, or execute a live test unless the user requested or authorized those effects.

## Required quality bar

- Name checks after behavior and evidence, not implementation tickets.
- Prefer OpenTelemetry semantic conventions and stable domain attributes over incidental span names.
- Use `resource["service.name"]`, `span.kind`, and a stable operation or domain attribute together when one field alone is ambiguous.
- Keep span identity in `matching`; keep behavioral expectations in named `assertions`.
- Prefer `at_least` for required participation. Use `exactly` only when cardinality is itself a contract, and `at_most` or zero only for meaningful negative guarantees.
- Base latency limits on an explicit SLO, requirement, or measured baseline with justified headroom. Never invent a threshold.
- Account for asynchronous export and side effects with measured observation windows. Do not increase timeouts blindly.
- Avoid assertions on generated trace IDs, span IDs, timestamps, random values, deployment-specific instance IDs, or unstable ordering.
- Never put credentials or customer data in source, labels, examples, or diagnostics. Declare secret variables as sensitive and supply them through the environment.
- Do not emit syntax unsupported by the installed Lamplight version. In particular, do not translate Tracetest selector syntax, parent-child selectors, TraceQL, span events, links, or aggregate queries into Lamplight unless the target version documents them.

## Completion

Report separately:

- the behavioral contract encoded by the test;
- the expected telemetry learned from existing Wick definitions;
- the observed trace evidence used to choose predicates;
- backend query evidence, when a backend was actually inspected;
- validation results;
- execution results, if execution was authorized;
- anything not verified, including unavailable traces, unmeasured latency budgets, or unsupported relationship assertions.
