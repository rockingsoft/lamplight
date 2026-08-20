# Environment profiles

## Configuration model

Use one shared OpenTelemetry baseline plus explicit environment overlays:

```text
otel/
├── base.env
├── dev.env
├── traced.env
└── prod.env
```

These names are a project convention, not an OpenTelemetry requirement. The project's launcher, task runner, Compose file, Kubernetes manifests, or deployment system must deterministically load exactly one overlay after the base.

An optional selector such as `OTEL_PROFILE=dev|traced|prod` can make that choice ergonomic, but the OpenTelemetry SDK does not interpret `OTEL_PROFILE`. Document and test the code or script that maps it to configuration. Prefer the repository's existing configuration system over introducing another loader.

Commit non-secret defaults and variable names. Inject endpoints, tenant headers, certificates, and tokens through the environment or secret manager. Provide an example file, not real credentials.

## Shared baseline

Keep identity and propagation consistent across profiles:

```sh
OTEL_SERVICE_NAME=checkout
OTEL_PROPAGATORS=tracecontext,baggage
OTEL_TRACES_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
```

Set resource attributes from deploy metadata where possible:

```sh
OTEL_RESOURCE_ATTRIBUTES=service.namespace=commerce,service.version=${APP_VERSION},deployment.environment.name=${DEPLOY_ENV}
```

Confirm that the target SDK supports variable expansion in the mechanism that loads this value. Plain environment variables do not recursively expand placeholders by specification; the shell, templating system, or orchestrator must do it.

## `dev`

Goal: useful local traces with simple setup and low debugging friction.

Typical overlay:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4318
OTEL_TRACES_SAMPLER=parentbased_always_on
OTEL_LOG_LEVEL=info
```

Use a local Collector or compatible backend. Keep console exporting and `OTEL_LOG_LEVEL=debug` as temporary opt-ins because they are noisy and can expose telemetry in terminal logs. A developer must be able to disable telemetry without code changes when the SDK supports `OTEL_SDK_DISABLED` or the project's launcher provides an equivalent switch.

## `traced`

Goal: reliably preserve the end-to-end trace initiated by Lamplight or another trace-based test runner.

Typical overlay:

```sh
OTEL_TRACES_SAMPLER=parentbased_always_on
OTEL_LOG_LEVEL=info
```

Choose the endpoint from the Lamplight datasource mode:

- For a direct-query backend such as Tempo, export through the normal Collector or backend ingestion endpoint. Lamplight queries the backend separately by trace ID.
- For a Lamplight embedded OTLP datasource, export to the OTLP listener configured for that run.

The test request carries a sampled W3C parent. Every service must extract it and use a parent-based sampler so the sampled decision survives downstream. Do not globally force unrelated production traffic to `always_on` merely to support tests; use a dedicated environment, workload, routing rule, or carefully reviewed sampling policy.

For short-lived test processes, verify graceful SDK shutdown and exporter flush. Set an observation window in Lamplight based on measured application, exporter, Collector, and backend delay.

## `prod`

Goal: reliable, bounded-overhead telemetry suitable for operations.

Typical application overlay:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
OTEL_TRACES_SAMPLER=parentbased_traceidratio
OTEL_TRACES_SAMPLER_ARG=${OTEL_PROD_TRACE_RATIO}
OTEL_LOG_LEVEL=info
```

Choose the ratio from traffic, cost, diagnostic requirements, and existing observability policy. Do not invent or silently change it. A parent-based ratio sampler continues an upstream sampled trace while applying the ratio to local roots.

Prefer a nearby Collector with batching, sending queue, retry, memory protection, TLS, authentication, and monitored self-telemetry. Avoid console exporters and verbose SDK diagnostics. Treat `always_on` as an explicit capacity and cost decision, not a default recommendation.

If tail sampling is required, design it at the Collector layer and verify that head sampling does not discard traces before they reach the tail sampler. This is an architecture decision and should not be introduced as a small instrumentation edit.

## Profile verification matrix

| Property | dev | traced | prod |
| --- | --- | --- | --- |
| Startup hook loads | required | required | required |
| Stable service identity | required | required | required |
| Trace Context propagation | required | required | required |
| Export path proven with a real trace | required | required | required |
| Incoming Lamplight trace ID retained | useful | required | when tests target prod |
| Sampling | parent-based, normally all on | parent-based, preserve sampled parent | policy-driven parent-based sampling |
| SDK debug logging | temporary opt-in | temporary opt-in | off by default |
| Collector | local or optional | depends on datasource path | recommended |
| Secrets committed | never | never | never |

Test the selector itself: choosing one profile must not accidentally merge contradictory endpoints, protocols, samplers, or credentials from another profile.

