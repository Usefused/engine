# Telemetry

Engine uses OpenTelemetry for traces, metrics, and logs when OTLP endpoints are
configured. If no OTLP endpoint is configured, traces run as no-op, metrics use
a no-op provider, and logs go to stderr.

## Configuration

Common variables:

- `OTEL_SERVICE_NAME`: service name, usually `engine`.
- `OTEL_EXPORTER_OTLP_ENDPOINT`: shared OTLP HTTP endpoint for traces, metrics,
  and logs.
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`: optional traces-specific HTTP endpoint.
- `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`: optional logs-specific HTTP endpoint.
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

Signal-specific environment endpoints take precedence over the shared endpoint,
which takes precedence over `observability.otel_target` in `engine.yaml`. The
YAML fallback must be an OTLP/HTTP target, normally `http://localhost:4318`.

Leave the OTLP endpoint variables unset to disable export.

The repository's local Compose stack sends all three signals to an OpenTelemetry
Collector. The collector forwards traces to Jaeger and accepts bounded logs and
metrics through its debug exporter; Jaeger itself is trace-only and must not be
used as the log endpoint.

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

## Execution receipts and sessions

SDK and MCP Activity show one parent receipt for each admitted Unified call,
with the existing service receipts beneath it. The parent records total elapsed
time and bounded forward/rollback outcomes; it does not retain inputs or
responses, and it does not add another provider usage count. Selecting a child
opens its normal receipt in the same sidebar. Back restores the Unified view.
Rejected calls that fail whole-call preflight do not create a parent receipt.
Provider timing capture and parent/child linkage work without an OTLP exporter.

Sessions use server-side cursor pagination. New sessions retain bounded
client-reported `initialize.clientInfo.name` and `version`, plus the initial
observed client IP. These fields are visible only through app/audit-authorized
Activity; they are not added to traces, logs, or metrics. Historical missing
values display “Not recorded.” Hard-deactivated app versions retain the existing
session-deletion behavior; this change does not extend session retention.

By default, the initial IP is the direct HTTP peer. Behind a reverse proxy, set
`FUSED_MCP_TRUSTED_PROXY_CIDRS` to a comma-separated list of the actual trusted
proxy networks, for example `192.0.2.10/32,2001:db8:1::/64`. Only a trusted peer
may supply `X-Forwarded-For`; Engine walks the chain from the right and stops at
the first untrusted hop. Invalid configuration or chain evidence falls back to
the direct peer. Do not configure every address as trusted. VPNs, NAT and hosted
clients can expose an intermediary address, so this is provenance, not identity
verification. The MCP client name/version are also self-reported.

### Upgrade coordination

This release adds schema migration 13, execution-event envelope version 6, and
an `initialized` session transition with additive metadata. New workers accept
old version-5 execution events and historical session events. Old workers do
not accept the new documents and can discard them if they share the queue.
Drain/stop old Engine producers and consumers before starting the new binaries;
do not overlap old and new replicas on the same execution/session consumer
queues. Engine startup applies the forward migration automatically. No
historical client metadata or detailed timings can be recovered retroactively.
