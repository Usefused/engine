# License Key

`FUSED_LICENSE_KEY` is required to start Fused Engine.

Fused Engine is source-available under the PolyForm Noncommercial License
1.0.0. Commercial or production use requires a separate written agreement with
Fused in addition to a valid license key.

The key identifies the licensed account/workspace that this Engine instance is
allowed to serve. On startup, Engine sends it to Registry at
`/api/engine/handshake`. Registry returns the account ID and workspace name;
Engine mirrors that workspace in local Postgres and caches the key for local
runtime API-key validation.

## What Happens Without A Key

Engine exits during startup before serving HTTP, webhook, or gRPC traffic.
There is no unsupported local/offline mode that bypasses Registry.

## What The Key Is Used For

Engine uses the license key for Engine-owned calls:

- startup handshake
- signed heartbeat checks that keep the workspace marked as a verified runtime
- signed aggregate usage reports
- background runtime metadata fetches
- service changelog polling when there is no caller request context
- cache misses for runtime contract and endpoint metadata

For caller-initiated Registry proxy routes, Engine forwards the caller's
`X-API-Key` to Registry. The license key is not substituted for user
authorization.

## Storage

The license key is read from `FUSED_LICENSE_KEY`, the `--license-key` flag, or
configuration loaded by Engine. Prefer environment variables or secret managers
in production. Do not commit production license keys to `engine.yaml`.
