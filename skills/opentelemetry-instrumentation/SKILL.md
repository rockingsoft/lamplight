---
name: opentelemetry-instrumentation
description: Instrument applications with OpenTelemetry for production debugging and trace-based testing. Use when adding or auditing automatic instrumentation, SDK initialization, OTLP exporters, context propagation, Collector connectivity, environment-specific telemetry profiles, or useful custom spans. Do not use merely to query an observability backend or author Lamplight tests.
license: MIT
---

# OpenTelemetry instrumentation

Instrument the application so its traces are useful both for operating the system and for trace-based tests. Prefer official automatic instrumentation and instrumentation libraries. Add manual spans only where they provide missing domain meaning or asynchronous context.

## Establish the current state

Inspect the repository and its runtime before changing it:

1. Detect every deployable service, language, framework, entrypoint, dependency manager, and deployment method in scope.
2. Inventory existing OpenTelemetry APIs, SDKs, agents, instrumentation libraries, exporters, Collector configuration, environment variables, and backend integration.
3. Follow one representative request through inbound handling, internal business work, outbound HTTP/RPC, database, messaging, and background processing.
4. Separate confirmed gaps from unverified assumptions. Preserve working instrumentation and avoid initializing a second SDK or provider.

Read [references/setup-workflow.md](references/setup-workflow.md) when installing or repairing automatic instrumentation, SDK/exporter setup, propagation, or Collector connectivity.

Read [references/environment-profiles.md](references/environment-profiles.md) when introducing or changing the `dev`, `traced`, and `prod` configuration model.

Read [references/span-design.md](references/span-design.md) before adding or reviewing manual spans and attributes.

## Implementation order

Apply the smallest sufficient layer in this order:

1. Official zero-code or automatic instrumentation for the target language and runtime.
2. Official instrumentation libraries for frameworks, HTTP/RPC clients and servers, databases, and messaging libraries.
3. One correctly initialized SDK and resource identity when automatic setup does not provide it.
4. OTLP exporter and Collector or backend connectivity.
5. W3C Trace Context propagation across every supported boundary.
6. Manual application spans only for meaningful operations not already represented.

Do not replace automatic spans with handwritten wrappers or duplicate spans already emitted by a framework, client, ORM, driver, or messaging instrumentation.

## Source requirements

OpenTelemetry packages, supported libraries, setup hooks, defaults, environment-variable support, and semantic conventions change over time. Before editing dependencies or startup commands, consult the current official OpenTelemetry documentation for the detected language and the official documentation for the specific instrumentation library. Use vendor documentation only for the final backend-specific export boundary.

Record the versions and documentation used when behavior depends on them. Never assume every SDK implements every standard environment variable.

## Verification

Verify observable behavior, not only compilation:

1. Run the repository's normal formatting, unit, and integration checks.
2. Start the service under the selected telemetry profile when execution is authorized.
3. Trigger one real operation through its normal entry boundary.
4. Confirm a server or consumer span, expected automatic dependency spans, propagation across service boundaries, resource identity, exporter delivery, and any new domain span.
5. Confirm error recording with a safe failing operation when appropriate.
6. For `traced`, confirm the trace ID received from Lamplight remains the trace ID exported by downstream services.
7. Check for duplicate spans, missing parents, accidental new roots, sensitive attributes, exporter errors, and unexpected telemetry volume.

Do not declare instrumentation complete from application logs saying an exporter started. A successful verification includes backend or collector evidence for a real trace.

## Safety and scope

- Never commit exporter credentials, tenant headers, private endpoints, or customer payloads.
- Do not enable verbose SDK diagnostics in production by default.
- Do not silently change production sampling, retention, Collector pipelines, or telemetry costs.
- Preserve authorization boundaries before deploying, restarting shared services, or mutating a live backend.
- Treat telemetry as production data: minimize attributes and redact or omit secrets, tokens, full request bodies, raw SQL values, and personal data.

## Completion report

Distinguish:

- automatic coverage added or confirmed;
- SDK, exporter, propagator, Collector, and resource configuration;
- profile behavior for `dev`, `traced`, and `prod`;
- manual spans and the specific observability gap each fills;
- local validation, live export verification, and anything not tested;
- expected production overhead, sampling, and rollout considerations.

