# Registry Contract

Fused Engine is a source-available, licensed runtime. The Fused Cloud Registry remains the control plane and
source of truth for catalogue data, account binding, and user authorization.
Engine treats Registry as a remote dependency only; it must not import Registry
packages or initialize Registry-owned tables.

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

Engine uses the license key for Engine-owned background calls:

- startup handshake with `/api/engine/handshake`
- signed heartbeat checks with `/api/engine/heartbeat`
- signed aggregate usage reports with `/api/engine/usage-reports`
- runtime contract fetches used to execute configured SDK/MCP calls
- metadata fetches used to populate local runtime caches
- catalogue search and service operation lookups

User/API requests that are proxied to Registry keep the caller's own
`X-API-Key`. Engine validates local access before proxying where needed, but it
does not replace the caller key with the license key for Registry-owned user
operations.

## Registry-Owned Data

Registry owns:

- accounts and entitlements
- service catalogue records
- service versions and version revisions
- integration objects and generated SDK metadata
- import/apply and SDK generation workflows
- published drift/changelog metadata
- provider-owned baseline connection profile revisions

Engine may cache snapshots of some Registry data locally so runtime execution
does not need to query Registry on every request.

## Entitlement And Runtime Verification

The startup handshake returns a minimal entitlement bundle:

- `plan`
- `heartbeat_required`
- `usage_reporting`
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

## Engine-Owned Data

Engine owns local runtime state in its Postgres database:

- licensed workspace mirror from the startup handshake
- local API key cache
- buckets, bucket values, secrets, and webhook configs
- SDK scope bindings and service contract snapshots
- service changelog cache and workspace notifications
- OAuth/connect sessions, encrypted token material, and connection resources
- MCP/webhook analytics and execution audit records

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
  changes through caller-authenticated Registry endpoints.
- Changelog/drift: poll Registry for service changelog entries and drift
  snapshots so Engine can create workspace-local notifications.
- Proxy routes: forward Registry-owned GraphQL and REST requests, preserving the
  caller API key.
- Generation/import intercepts: proxy Registry generation/import requests while
  materializing the Engine-local runtime state needed by the workspace.

Runtime vendor calls made by SDK/MCP/webhook execution go to the configured
third-party API, not to Registry, except when Engine needs Registry metadata
that is missing from its local cache.
