# Registry Contract

Fused Engine is a source-available, licensed runtime. The Fused Cloud Registry
remains the source of truth for catalogue data, account binding, and
entitlements. Engine owns workspace-local identity and authorization. It treats
Registry as a remote dependency only; it must not import Registry packages or
initialize Registry-owned tables.

## Configuration

Engine reaches Registry through `FUSED_REGISTRY_ENDPOINT` or
`engine.registry_endpoint` in `engine.yaml`. The default endpoint is the Fused
Cloud Registry; overrides are for Fused support and internal development.

The endpoint may be either a Registry base URL or a GraphQL URL. Engine trims a
trailing `/graphql` when it needs REST endpoints such as
`/api/engine/handshake`.

## Authentication

`FUSED_LICENSE_KEY` is required at startup. Without it, Engine exits before
serving traffic.

Engine uses the license key for all Registry calls:

- startup handshake with `/api/engine/handshake`
- signed heartbeat checks with `/api/engine/heartbeat`
- signed aggregate usage reports with `/api/engine/usage-reports`
- runtime contract fetches used to execute configured SDK/MCP calls
- metadata fetches used to populate local runtime caches
- catalogue search and service operation lookups
- locally authorized control requests proxied to Registry

At startup, the same key creates or reconciles the workspace's local bootstrap
Owner. Engine stores only its cryptographic hash in the local credential table.
Future personal user and service-account credentials remain local: after Engine
authentication and authorization, proxy code strips the inbound credential and
injects `FUSED_LICENSE_KEY` for the Registry request.

## Registry-Owned Data

Registry owns:

- accounts and entitlements
- service catalogue records
- service versions and version revisions
- integration objects and generated SDK metadata
- import/apply and SDK generation workflows
- SDK archive lifecycle: `DELETE /sdks/{artifact_id}` is idempotent; Registry
  returns success when retired and `404` when already absent. Engine treats
  both as retired, but preserves its local runtime/config references on any
  other Registry status so the caller can retry safely.
- published drift/changelog metadata
- provider-owned baseline connection profile revisions

Engine may cache snapshots of some Registry data locally so runtime execution
does not need to query Registry on every request.

## Entitlement And Runtime Verification

The startup handshake returns a minimal entitlement bundle:

- `plan`
- `heartbeat_required`
- `usage_reporting`
- `public_service_insights_reporting`
- `heartbeat_interval_seconds`
- `heartbeat_stale_after_seconds`

Engine persists the latest bundle locally so operators can debug which
commercial/runtime contract the process is following during transient Registry
outages. Older Registries that omit these fields are treated as the default
commercial contract: heartbeats required, aggregate usage reporting, one-minute
heartbeat interval, and five-minute heartbeat staleness.

Heartbeats are required for licensed self-hosted commercial runtimes because
Registry is the source of truth for whether an account currently has a verified
Engine online. Missed heartbeats do not immediately stop local runtime calls;
Registry marks the account `unverified_runtime` so cloud/support surfaces can
degrade or warn without turning transient network issues into runtime outages.

## Aggregate Usage Reporting

Engine stores local aggregate counters by metric and time bucket, then flushes
idempotent report rows to Registry. Reports contain only:

- report identity
- metric name from a closed vocabulary
- time bucket
- count
- Engine version/build identity

Reports never include request payloads, response bodies, headers, provider
URLs, credentials, or end-user references. Pricing and feature enforcement are
intentionally deferred until product plans exist; the first contract records the
safe accounting path without inventing billing decisions in Engine.

## Public-Service Insights

Public-service insights are a separate product-observability contract and do
not share tables or payloads with commercial usage reporting. When the
`public_service_insights_reporting` entitlement is enabled, Engine uses signed
requests for:

- `POST /api/engine/public-service-insight-eligibility`
- `POST /api/engine/public-service-insight-reports`
- `POST /api/engine/public-service-insights/query`

Engine first asks Registry which parent services are public. It then derives
closed-hour aggregates from canonical local execution events and writes them to
a durable outbox before delivery. Registry validates parent visibility and the
service, version, and Registry-object relationship again; a public version of a
private service is never reportable.

Reports contain counts, bounded dimensions, latency sums, fixed histogram
buckets, and retry totals. They cannot contain environment names, local
artifact identities, users, provider URLs, traces, payloads, secrets, or raw
failure messages. The current cross-Engine projection reports endpoint calls;
webhook calls remain fully available in local Activity until canonical webhook
events carry a stable Registry webhook-object identity.

Owner-only aggregate reads also travel through Engine. The embedded UI calls
Engine GraphQL, Engine applies local `service.read` and `audit.read`, Registry
independently verifies service ownership, and Engine may return a short-lived
cached result during a Registry outage. Local Activity does not depend on this
cloud path.

## Engine-Owned Data

Engine owns local runtime state in its Postgres database:

- licensed workspace mirror from the startup handshake
- local subjects, credentials, roles, team bindings, and authorization revision
- secret-safe local authorization audit events
- buckets, bucket values, secrets, and webhook configs
- SDK scope bindings and service contract snapshots
- service changelog cache and workspace notifications
- OAuth/connect sessions, encrypted token material, and connection resources
- canonical SDK, MCP, and webhook execution history
- the durable public-service insight projection outbox

Engine tables are created by the Engine schema code under `internal/shared/db`.
Registry schema helpers are intentionally not present in this repository.

## Outbound Registry Calls

Engine calls Registry for these classes of work:

- Handshake: bind the Engine to the licensed account/workspace.
- Runtime verification and accounting: send signed heartbeats and aggregate
  usage reports under the Registry-issued entitlement contract.
- Runtime metadata: fetch service metadata, endpoint definitions, runtime
  contracts, version revisions, auth configs, and connection profile contracts.
- Workspace configuration: verify service/version references, resolve slugs,
  materialize runtime contract snapshots, and publish allowed configuration
  changes using the Engine licence identity after local authorization.
- Changelog/drift: poll Registry for service changelog entries and drift
  snapshots so Engine can create workspace-local notifications.
- Proxy routes: forward Registry-owned GraphQL and REST requests after local
  authorization, replacing any local caller credential with the Engine licence
  identity.
- Generation/import intercepts: proxy Registry generation/import requests while
  materializing the Engine-local runtime state needed by the workspace.

Runtime vendor calls made by SDK/MCP/webhook execution go to the configured
third-party API, not to Registry, except when Engine needs Registry metadata
that is missing from its local cache.
