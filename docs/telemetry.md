# Telemetry

Engine uses OpenTelemetry for traces, metrics, and logs when OTLP endpoints are
configured. If no OTLP endpoint is configured, traces run as no-op, metrics use
a no-op provider, and logs go to stderr.

## Configuration

Common variables:

- `OTEL_SERVICE_NAME`: service name, usually `engine`.
- `OTEL_EXPORTER_OTLP_ENDPOINT`: shared OTLP HTTP endpoint for traces, metrics,
  and logs.
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`: optional metrics-specific endpoint. It
  takes precedence over the shared endpoint for metrics.
- `FUSED_ENGINE_ENVIRONMENT`: deployment label attached to telemetry, default
  `production`.

Examples:

```bash
OTEL_SERVICE_NAME=engine
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 # Works perfectly with Threadify
FUSED_ENGINE_ENVIRONMENT=staging
```

Leave the OTLP endpoint variables unset to disable export.

## What Is Recorded

Engine records operational metadata such as route class, service IDs, endpoint
names, status/outcome labels, retry counts, pagination counts, webhook
verification events, cache timings, and execution audit correlation IDs.

User/agent-triggered executions create trace spans so operators can debug why a
runtime call was allowed, retried, rejected, or failed. The execution path also
adds an aggregate-only span event when commercial usage counters are enqueued;
the event records bucket/status metadata only, not payload content.

## Secret Handling

Credentials and secrets must not be added to span attributes, refs, logs, or
metrics. Credential resolution and dispatch paths intentionally record
identifiers and aggregate counts instead of credential values. Runtime payloads
may contain PII, so handlers should avoid exporting raw request/response bodies
unless a future feature explicitly adds redaction and opt-in controls.
