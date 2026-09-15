# Threat Model

This document describes the security boundary for Fused Engine as a
source-available, self-hosted, licensed runtime.

## Assets

- `FUSED_LICENSE_KEY`
- `FUSED_ENCRYPTION_KEY`
- workspace secrets and bucket values
- OAuth/OIDC client secrets, refresh tokens, and access tokens
- webhook signing secrets
- SDK tokens and API keys
- runtime request/response payloads
- local Postgres and NATS state

## Trust Boundaries

Engine runs in the customer's infrastructure. Registry is a remote Fused control
plane. Third-party APIs are external vendor systems. The embedded UI is served
by Engine and calls Engine APIs.

Registry owns account entitlements, catalogue services and versions, import and
generation workflows, and provider baseline connection profiles. Engine owns
workspace identities and authorization, buckets and provider credentials,
configured apps and execution tokens, exact service-version snapshots, and
runtime activity.

Engine materializes Registry contracts during explicit control-plane workflows.
Configured SDK, MCP, and webhook execution then uses those exact local snapshots
plus applied workspace overrides. Missing or invalid local state fails closed;
runtime execution never falls back to current Registry metadata.

Engine does not scrape arbitrary network traffic. It injects credentials only
when handling Fused-managed SDK, MCP, webhook, proxy, or workspace routes that
have already resolved a configured service/bucket/auth binding.

## Network Calls

Engine makes outbound calls to:

- Registry for licensing, explicit catalogue and contract acquisition,
  workspace configuration, import and generation, changelog and drift polling,
  Registry-owned proxy workflows, and privacy-bounded aggregate reporting. See
  `docs/license-key.md` and `docs/telemetry.md`.
- Third-party APIs during configured SDK/MCP/runtime execution.
- OAuth/OIDC providers during connect-session authorization and token refresh.
- OTLP collectors when telemetry endpoints are configured.
- An operator-configured external NATS cluster when Engine replicas share
  JetStream state.

Inbound traffic includes:

- Engine API and embedded UI on the configured HTTP port.
- gRPC SDK connections on the configured gRPC port.
- webhook ingress on the configured webhook port or shared HTTP port.
- local NATS connections when embedded NATS is enabled.

Embedded NATS binds to loopback and is a single-instance convenience. External
NATS credentials are supplied separately from `NATS_URL`; Engine logs only the
bounded connection mode, authentication method, and TLS state. Independent
Engine installations must not share a NATS account or fixed stream namespace.

## Credential Storage

Secrets are stored in Engine Postgres and encrypted using configured key
material from `FUSED_ENCRYPTION_KEY`. Production deployments must provide their
own key. The checked-in example/default key is publicly known and only suitable
for local throwaway development.

OAuth tokens and webhook secrets stay in the Engine deployment. Registry stores
catalogue and configuration metadata, not Engine-local encrypted token
material.

## Telemetry

Telemetry is disabled unless OTLP endpoints are configured. When enabled,
Engine exports operational metadata, spans, logs, and metrics. Credentials must
not be placed in telemetry. See `docs/telemetry.md`.

## Expected Failure Modes

- Missing `FUSED_LICENSE_KEY`: Engine exits during startup.
- Failed Registry handshake: Engine exits during startup.
- Registry unavailable after startup: configured execution may continue from
  exact local snapshots during the heartbeat grace period. Missing local state
  and Registry-backed control workflows fail closed. When heartbeat verification
  is required, expiry of the local lease blocks runtime traffic while leaving
  recovery and control routes available; the next successful heartbeat reopens
  it. See `docs/license-key.md`.
- Missing `FUSED_ENCRYPTION_KEY`: startup secret loading fails where encrypted
  credential features require it, or development defaults warn loudly.

## Non-Goals

- Running Engine without Registry/account licensing.
- Acting as a generic credential-injecting HTTP proxy.
- Publishing Registry private schema, billing, or account provisioning internals
  in this repository.
