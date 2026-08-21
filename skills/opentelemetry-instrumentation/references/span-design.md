# Designing useful spans

## Add manual spans only for a gap

First inspect the trace produced by automatic instrumentation. Add a manual span when a meaningful operation with duration is otherwise invisible, such as:

- a domain command or workflow stage;
- validation or policy evaluation important to failures;
- an asynchronous job or consumer operation not covered by instrumentation;
- a call through a custom protocol;
- a logical operation spanning several automatically instrumented dependencies.

Do not add a span for every function. Do not duplicate HTTP, RPC, database, cache, or messaging spans already emitted by official instrumentation. Use a span event or ordinary correlated log for an important point-in-time occurrence.

## Span boundary

A span should represent one operation with a clear start, end, and outcome. Start it immediately before the operation and end it on every success, error, cancellation, and early-return path. Make it a child of the current context unless the operation is genuinely a new root.

For asynchronous execution, explicitly carry the context through the supported transport. Use a consumer child when one message causes one operation. Consider span links for batches, fan-in, retries represented as separate causal attempts, or work with multiple parents, following the current semantic convention and SDK guidance.

## Naming

Use a low-cardinality operation name that groups comparable work:

```text
order validate
order persist
payment authorize
inventory reserve
```

Prefer the OpenTelemetry semantic-convention naming pattern for known protocols. Never put order IDs, user IDs, URLs with raw path parameters, SQL statements, timestamps, error messages, or random values in the span name. Put safe dimensions in attributes.

## Attributes

Use the current OpenTelemetry semantic conventions before inventing custom names. For application-specific attributes:

- use a stable organization or domain namespace such as `acme.order.operation`;
- use lowercase dot-separated namespaces and snake_case components;
- preserve the correct scalar or homogeneous-array type;
- keep values bounded enough for useful grouping;
- set attributes needed for head sampling at span creation time;
- document their meaning and stability if tests depend on them.

Useful attributes explain what kind of operation occurred and its safe outcome. Examples include a domain operation type, queue or workflow stage, retry attempt bounded to a small number, result category, or feature mode.

Avoid secrets, authentication material, request and response bodies, raw SQL values, payment data, personal data, unbounded exception messages, arbitrary headers, and high-cardinality identifiers unless there is a reviewed operational need and data policy.

Trace-based tests need stable evidence, not necessarily unique business identifiers. If an identifier is essential for debugging, consider whether it can be hashed, truncated, classified, attached only under controlled sampling, or kept in correlated secure logs instead.

## Status, errors, and events

Follow the semantic convention for the operation. Record an exception using the SDK's standard error or exception mechanism and set span status to error when the operation itself failed. Do not mark a span as error merely because an expected business outcome occurred, such as a validation rejection, unless the operation's convention defines it as failure.

Do not record the same exception independently on every ancestor span. Let each span describe its own operation. Add concise safe attributes that classify the failure; keep stack traces and messages subject to the application's telemetry data policy.

Use events for meaningful occurrences within a longer operation, especially when their timestamp matters but they do not have duration. Avoid events that merely restate logs without adding trace-local diagnostic value.

## Debugging and testing value

A useful trace should let an operator or test author answer:

- Which logical service and operation handled the request?
- Which significant domain stages ran?
- Which external dependencies or asynchronous effects occurred?
- Where did time go?
- What failed, and how was the failure classified?
- Was context preserved across every boundary?
- Which stable attributes can select this operation across environments?

When a trace-based test will depend on a manual span, treat its name, kind, and selected attributes as an observable contract. Add an instrumentation test that checks propagation and essential fields without snapshotting every incidental attribute.

If the repository already has Lamplight Wick tests, use their `matching`,
`span_assertions`, quantity, and observation-window fields to understand the
existing contract before renaming a span or attribute. Verify those
expectations against actual backend telemetry. Preserve meaningful contracts,
but do not duplicate spans or retain misleading telemetry solely because a
stale Wick still selects it; update the owning contract when intent changed.

## Review checklist

- Automatic and library instrumentation was evaluated first.
- The span fills a named observability gap and has meaningful duration.
- It uses current context and ends on every path.
- Its name is stable and low-cardinality.
- Its kind matches the represented operation.
- Standard semantic attributes are used where available.
- Custom attributes are documented, typed, bounded, and non-sensitive.
- Error status describes the operation rather than blindly mirroring an exception.
- It does not duplicate an existing span.
- It helps both incident debugging and resilient trace assertions.
