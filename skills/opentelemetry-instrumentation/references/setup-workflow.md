# OpenTelemetry setup workflow

## 1. Detect the runtime and official path

Identify the exact language and runtime version before choosing packages or startup hooks. OpenTelemetry supports automatic or zero-code instrumentation through different mechanisms: agents, profilers, preload modules, monkey patching, eBPF, or language-specific launchers.

Consult the current official pages under:

- `https://opentelemetry.io/docs/zero-code/`
- `https://opentelemetry.io/docs/languages/`
- `https://opentelemetry.io/ecosystem/registry/`

Check compatibility with the repository's framework and library versions. Prefer stable official packages. Do not copy package names or commands from another language or an old example.

## 2. Automatic instrumentation first

Enable the official automatic instrumentation and observe a representative operation before writing application instrumentation. Look for:

- inbound server or messaging consumer spans;
- outbound HTTP and RPC client spans;
- database and cache spans;
- messaging producer and consumer spans;
- correct parent-child context across asynchronous work;
- `service.name` and other resource identity.

Automatic instrumentation describes the edges of application code particularly well. It usually does not describe domain operations inside the application, which is where carefully chosen manual spans may help.

Avoid installing overlapping instrumentations for the same library. If two agents or SDK initializers are present, determine which owns the provider and remove duplication only when the requested change authorizes it.

## 3. SDK ownership

There must be one effective tracer provider or SDK initialization per process. Determine whether it is owned by:

- the automatic instrumentation agent or launcher;
- framework integration;
- an existing application bootstrap module;
- explicit SDK code.

Use the OpenTelemetry API in ordinary application and library code. Keep SDK construction, processors, exporters, and shutdown in the application composition root. A reusable library should not install a global SDK or exporter.

Register shutdown or flush behavior appropriate to the runtime so buffered spans are not routinely lost. Do not flush synchronously after every span.

## 4. Resource identity

Every deployable service needs a stable, low-cardinality identity. At minimum set `service.name`. Add fields such as service namespace, version, and deployment environment when the runtime and semantic conventions support them.

A portable baseline is:

```sh
OTEL_SERVICE_NAME=checkout
OTEL_RESOURCE_ATTRIBUTES=service.namespace=commerce,service.version=1.8.0,deployment.environment.name=dev
```

Do not put pod UID, request ID, user ID, commit message, or another high-cardinality value into `service.name`. Runtime resource detectors can attach host, container, cloud, and Kubernetes identity without changing the logical service name.

## 5. Context propagation

Use W3C Trace Context and baggage unless the system has a documented interoperability requirement:

```sh
OTEL_PROPAGATORS=tracecontext,baggage
```

Confirm extraction at inbound boundaries and injection at outbound boundaries. Automatic instrumentation should own propagation for supported HTTP, RPC, and messaging libraries. Add manual propagation only for unsupported transports or custom envelopes.

For asynchronous messages, inject context into message metadata before publish and extract it before starting consumer work. Do not copy an ambient context after it has ended. Use links instead of parentage when the processing model is genuinely batch, fan-in, or otherwise not a single causal child, following the target SDK's guidance.

Lamplight sends an authoritative sampled W3C `traceparent`. The application and downstream services must continue that context; starting a new root breaks trace correlation.

## 6. Export path

Prefer OTLP as the application export protocol. Configure it outside application code where the SDK supports standard configuration:

```sh
OTEL_TRACES_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
```

Endpoint semantics vary by protocol and SDK. With a signal-specific HTTP endpoint, `/v1/traces` may be required; with a base endpoint it may be appended automatically. Verify the current SDK documentation rather than guessing.

Keep authentication headers outside source control:

```sh
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20${OTEL_TOKEN}
```

Do not commit the expanded value. Confirm the SDK's required escaping rules and prefer the deployment platform's secret mechanism.

## 7. Collector and backend

For production, prefer exporting applications to an OpenTelemetry Collector close to the workload, then exporting from the Collector to the backend. This centralizes credentials, batching, retries, routing, enrichment, filtering, and vendor-specific configuration.

Check that:

- the OTLP receiver is declared and enabled in the traces pipeline;
- processors and exporters are referenced by that pipeline, not merely defined;
- TLS and authentication match on both sides;
- batching, sending queues, retry behavior, and memory limits match expected volume;
- Collector self-telemetry exposes refusals, drops, queue pressure, and exporter failures;
- no processor removes fields needed for debugging or trace-based tests.

Do not add a Collector if the requested scope is only application instrumentation and the existing direct OTLP path is intentional. Explain the tradeoff instead.

## 8. Diagnose by layer

When no trace appears, test each boundary:

1. Is the auto-instrumentation startup hook actually loaded?
2. Does the process have one initialized provider?
3. Is the relevant instrumentation library enabled and compatible?
4. Is the incoming context extracted and sampled?
5. Is the span ended and handed to a processor?
6. Does the exporter reach the configured endpoint using the configured protocol?
7. Does the Collector receive, process, and export it?
8. Does the backend ingest it under the expected tenant and service identity?

Use temporary SDK or Collector debug output only at the layer under investigation, then disable it.

