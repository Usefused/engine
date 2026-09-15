# License Key

`FUSED_LICENSE_KEY` is required to start Fused Engine.

Fused Engine is source-available under the PolyForm Noncommercial License
1.0.0. Commercial or production use requires a separate written agreement with
Fused in addition to a valid license key.

The key identifies the licensed account/workspace that this Engine instance is
allowed to serve. On startup, Engine sends it to Registry at
`/api/engine/handshake`. Registry returns the account ID and workspace name;
Engine mirrors that workspace in local Postgres and stores only the key's hash
as the local bootstrap Owner credential.

## What Happens Without A Key

Engine exits during startup before serving HTTP, webhook, or gRPC traffic.
There is no unsupported local/offline mode that bypasses Registry.

## What The Key Is Used For

Engine uses the license key for Engine-owned calls:

- startup handshake
- signed heartbeat checks that keep the workspace marked as a verified runtime
- signed aggregate usage and public-service insight reports
- service contract acquisition during workspace activation and refresh
- service changelog polling when there is no caller request context
- Registry-owned catalogue, configuration, import, generation, and proxy work

For caller-initiated Registry work, Engine first authenticates and authorizes the
local control credential, removes it from the outbound request, and uses the
license key for Registry authentication. Local control and app execution
credentials are never forwarded to Registry.

Runtime execution does not use the license key to recover missing contracts.
SDK, MCP, and webhook calls resolve from exact Engine-local service-version
snapshots. Missing or invalid local state fails closed and requires an explicit
refresh or re-apply.

## Runtime Verification And Cutoff

When heartbeats are required, Registry supplies the heartbeat interval and the
`heartbeat_stale_after_seconds` grace period. Older Registry versions default to
a one-minute interval and five-minute grace period.

After a successful startup handshake, Engine seeds its local lease and sends a
heartbeat immediately. Each successful heartbeat resets the lease. A failed
heartbeat does not interrupt execution during the grace period. After the grace
period elapses, Engine detects expiry on its next lease check, which runs no more
than five seconds apart, and blocks runtime entry points until a heartbeat
succeeds:

- SDK unary and streaming gRPC calls return `Unavailable` with
  `license verification expired`.
- MCP, webhook ingress, and direct app execution calls return HTTP `503` with
  `license verification expired`.
- Health, authentication, administrative, and other control-plane routes remain
  available for diagnosis and recovery.

Registry also marks stale installations `unverified_runtime` for cloud and
support surfaces. That Registry status does not itself block Registry requests;
the Engine-local lease controls the runtime cutoff. Registry suspension is a
separate immediate gate that blocks every non-recovery Engine route.

## Storage

The license key resolves in this order: `--license-key`, `FUSED_LICENSE_KEY` in
the local `.env`, `engine.license_key` in `engine.yaml`, then an inherited
`FUSED_LICENSE_KEY` process environment variable. `FUSED_API_KEY` is never a
Registry license source. Prefer a deployment-managed `.env` or secret-backed
configuration in production, and do not commit production keys to
`engine.yaml`.
