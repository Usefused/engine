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
- `OTEL_LOGS_EXPORTER=none`: disables OTLP log export, including the startup
  delivery check, even when a shared or logs-specific endpoint is configured.
  Console logs still go to stderr; traces and metrics keep their own configuration.
  Leave unset or set to `otlp` to retain endpoint-based log export.
- `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`: optional metrics-specific endpoint. It
  takes precedence over the shared endpoint for metrics.
- `FUSED_ENGINE_ENVIRONMENT`: deployment label attached to telemetry, default
  `production`.

Examples:

```bash
OTEL_SERVICE_NAME=engine
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318 # OpenTelemetry Collector
FUSED_ENGINE_ENVIRONMENT=staging
```

Signal-specific environment endpoints take precedence over the shared endpoint,
which takes precedence over `observability.otel_target` in `engine.yaml`. The
YAML fallback must be an OTLP/HTTP target, normally `http://localhost:4318`.

Leave the OTLP endpoint variables unset to disable export.

To keep an existing OTLP destination but stop draining logs to it, set
`OTEL_LOGS_EXPORTER=none` in the Engine process environment and restart the Engine.
This also overrides an endpoint supplied by `engine.yaml`. Threadify is not a
log receiver; use its tracing integration without sending OTLP logs to it.

The repository's local Compose stack sends all three signals to an OpenTelemetry
Collector. The collector forwards traces to Jaeger and accepts bounded logs and
metrics through its debug exporter; Jaeger itself is trace-only and must not be
used as the log endpoint.

## What Is Recorded

Engine records operational metadata such as route class, service IDs, endpoint
names, status/outcome labels, retry counts, pagination counts, webhook
verification events, cache timings, and execution audit correlation IDs.

User/agent-triggered executions create trace spans so operators can debug why a
runtime call was allowed, retried, rejected, or failed. The canonical execution
event records whether its durable JetStream publication succeeded; its consumer
commits the physical receipt and commercial usage counters together.

## Registry Aggregate Reporting

Registry reporting is independent of OTLP export. Under the entitlement
contract, Engine derives commercial usage from first-seen durable physical
execution events, stores the counters locally in the same transaction as their
receipts, and sends idempotent aggregates containing only a report ID, a metric
from a closed vocabulary, a time bucket, a count, and Engine build identity.

Public-service insights use a separate projection and payload. Engine reports
eligible public-service endpoint aggregates such as counts, bounded dimensions,
latency totals and histogram buckets, and retry totals. The entitlement gates
owner reads of those insights, not eligible contribution.

Neither reporting path includes request or response bodies, headers, provider
URLs, credentials, end-user references, local app identities, environment
names, traces, or raw failure messages. Local Activity remains available without
Registry reporting.

## Secret Handling

Credentials and secrets must not be added to span attributes, refs, logs, or
metrics. Credential resolution and dispatch paths intentionally record
identifiers and aggregate counts instead of credential values. Runtime payloads
may contain PII, so handlers should avoid exporting raw request/response bodies
unless a future feature explicitly adds redaction and opt-in controls.

## Execution receipts and sessions

SDK and MCP Activity show physical execution receipts with provider timing.
Timing capture works without an OTLP exporter.

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

## Unified App receipt details

Ordinary app/activity readers can inspect app identity, version, total duration,
measured execution phases, and recorded operation outcomes without private
payload access. Failed receipts retain an observed worker failure stage as a
fixed `unified_app_<phase>_failed` value in `failure_reason`; the UI translates
that value into a short explanation and marks the corresponding trace stage.
Absent stage evidence remains unknown, and interrupted provider work must not
imply that retrying is safe.

Authored exception messages (including `throw new Error("…")`) are returned in
`error.message` and displayed to app receipt readers. Credential failures include
a recovery command using the service slug and bucket name, with UUID fallback.
The inspector reads the retained message through
`GET /apps/{app_id}/executions/{execution_id}/failure`, requiring
`app.unified_app.read` and `audit.read`; messages do not enter execution events
or OTEL. Pre-upgrade results keep their original generic messages.

Raw provider errors, stacks, requests, and responses remain available only through
**Private diagnostics**, with `app.unified_app.diagnostics.read` checked by Engine
on every read. These payloads are never added to ordinary receipts or OTEL.
