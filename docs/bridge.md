# Fused Bridge

Fused Bridge already supports MQTT locally. Pair a Bridge installation with an Engine to select its local read and command capabilities in SDKs, MCP apps, and Unified Apps. SDKs can also receive selected local events through their durable receivers.

## Pairing and storage

Configure `engine.public_url` for the browser/OAuth HTTP origin and `engine.public_grpc_url` for the externally reachable Bridge gRPC endpoint. Use HTTPS in deployed environments; plain HTTP is for loopback development. Multi-replica Engines must share PostgreSQL and the internal NATS broker. Configure an external broker with `max_payload: 4194304` to accommodate encoded protocol frames; Engine's embedded broker uses that allowance.

Pair from the workspace’s Fused page in Bridge Studio. One connection covers the complete Bridge workspace: existing and new profiles appear automatically. Apps still select exact operations and immutable contracts, and app planning requires authority in the pairing’s exact Engine workspace. Separate environments can pair their workspaces with separate Engines. Temporary OAuth consent requires `bridge.manage`; the pairing request and response explicitly declare `scope: workspace`. Engine stores only the installation credential’s hash. Installation credentials cannot authenticate app execution or general control routes.

Protocol 1.1 adds an authoritative sorted profile inventory, acknowledged before manifests or events in each session generation. The complete snapshot withdraws profiles deleted while Bridge was disconnected, including an empty workspace. Profiles are initially offline after reconnect until their current manifests refresh. Stopped profiles retain their last known schemas for discovery but cannot execute. Newly created profiles can appear offline before their schemas are known. A manifest’s optional `state` is `online`, `offline`, or `removed`; historical manifests without state retain their original online interpretation without changing current lifecycle state.

Existing profile-scoped pairings migrate with `scope: profiles` and remain blocked until fresh workspace OAuth consent creates a new pairing. Migration never silently widens old consent. Reconnect from Bridge Studio and update app pins for the new pairing. The deprecated profile-selection endpoint rejects updates explicitly.

Bridge's embedded JetStream stores its hot queue under **`<bridge-install-directory>/data/hot`**, resolved from the installation, independent of the shell working directory. MQTT capture is persisted there before its upstream acknowledgement. A failed or offline Engine does not cause queued data to be silently evicted.

Engine acknowledges an event only after committing its original envelope and deduplication identity to PostgreSQL. Bridge may then delete its local queued copy. Engine's outbox hands that event to the existing durable SDK receiver stream, clearing the PostgreSQL payload only after the broker acknowledges it. A small content-hash tombstone remains so a late retry cannot recreate the event. Each SDK receiver owns its own durable acknowledgement; delivery is at least once, and handlers should be idempotent. The Engine receiver stream retains its existing 30-day retention policy. An Engine acknowledgement means durable acceptance, not completion of every downstream handler.

Pending Engine ingress is bounded per pairing at 100,000 events / 256 MiB. Capacity exhaustion rejects new acceptance so Bridge retains responsibility. Payloads are limited to 1 MiB and manifests to 2 MiB.

## Selecting capabilities

Use **Integrations → Bridges** in Engine, or:

```sh
fused-cli bridge list
fused-cli bridge show <pairing-id>
```

Use readable grouped selections in authored config:

```yaml
bridges:
  home:
    bridge: home-bridge
    profile: echo-live
    connections:
      echo-show-8:
        operations: [device.info, volume.set, speech.speak]
```

The outer key is an app-local alias; the Bridge name must match exactly one active workspace installation. Connection keys are saved names within the selected Profile. Set `operations: "*"` to select all currently advertised unary reads and commands on one connection. Each connection may independently use a list or wildcard. Events are excluded from the wildcard and require the explicit event binding format below. Empty, missing, duplicate or ambiguous selections fail planning. At most 100 Bridge capabilities may be selected after expansion.

Planning resolves names and snapshots the selected operations and schemas. Its summary lists each expanded operation. Apply rechecks that resolution and rejects a changed catalogue or destination with a request to plan again. Published apps keep their original pins; discovering new operations does not silently grant them to an existing app. Use a new version to expand an app's selection.

Unified Apps import typed Bridge calls alongside regular services:

```ts
import { bridges, services } from "@fused/operations";

const device = await bridges.home["echo-show-8"].device.info({});
await bridges.home["echo-show-8"].volume.set({ level: 25 });
```

Dotted operation names become nested methods. Type checking uses the exact selected input/output schemas and rejects unselected operations. Conflicting paths (such as selecting both `player` and `player.status`) fail planning instead of overwriting a method. The generated function invokes a private, deterministic alias through the existing Bridge effect recorder; caller input cannot replace its destination.

### Legacy explicit bindings

Explicit per-operation pins remain supported for existing consumers and SDK event subscriptions. Copy the returned `manifest_hash` directly. It hashes exact accepted bytes and is not interchangeable with a manifest revision or a digest of reformatted JSON.

Add a selection to an existing `kind: sdk`, `kind: mcp`, or `kind: unified_app` configuration:

```yaml
bridges:
  volume:
    pairing_id: <pairing UUID>
    profile_id: local
    manifest_hash: sha256:<64 hexadecimal characters>
    connection_id: speaker
    operation_id: set-volume
```

Normal app plan/apply resolves and freezes the operation contract. Each alias contributes to the same family-token capability expansion checks as other selected operations. Package rebuilds use the applied contracts. Provider credentials and connection configuration stay on Bridge.

Generated clients expose unary operations under `sdk.bridges.volume(...)` in TypeScript, `sdk.bridges.volume(...)` / `volume_sync(...)` in Python, and `sdk.Bridges.Volume(...)` in Go. MCP's existing `execute` tool accepts `{"operation":"bridge:volume","input":...}`. The app REST execution endpoint uses the same operation name. Unified App source calls `fused.callBridge("volume", input)`; its effect is recorded through the existing execution transcript.

Selected event operations produce `LocalBridgeEvent` descriptors in TypeScript/Python and typed `Bridge...Event` descriptors in Go. Register these with the ordinary generated webhook receiver; no provider webhook registration is required. Event selection is currently limited to `kind: sdk`; MCP event resources and automatic Unified App event triggers are not exposed by this adapter. Subscriptions recheck both the app's token restriction (`bridge:<alias>`) and the live installation grant. Event capabilities are not callable commands.

## Replay and revocation

An immutable app pin may continue calling an unchanged operation after unrelated manifest updates. Removed operations or changed contract hashes fail closed. Historical manifests support replay without replacing the current catalogue.

Ordinary calls remain live and are not automatically resent by the Bridge adapters. Unified Apps can enable `snapshot: true` with a recovery policy. Engine binds a stable logical run/call identity to the exact app, destination, operation, contract and input. Successful finite Bridge responses use the existing Engine idempotency table and logical-run retention. Recovery returns a saved response without requiring a live Bridge, while still checking app scope, installation consent and the current contract.

When Engine has no saved response, the stable idempotency key travels in the gRPC `Call`. A Bridge advertising `Hello.durable_calls` atomically claims it in its private, fsync-backed JetStream journal before invoking the connector, then commits the result before replying. A repeated key returns the stored result; changed input or routing is rejected. Lost Engine commits can therefore retrieve Bridge's result without repeating the action. Older Bridges remain usable for ordinary calls, but reject snapshot dispatch before any side effect.

A pending record left by a crash, or an explicitly unknown outcome, is never automatically repeated. Only a confirmed `not_started` outcome releases the key for another attempt. The journal has no automatic expiry or eviction: it shares the bounded installation storage quota and fails closed when full. Call deadlines remain enforced; this does not introduce an offline command queue. A device action performed before Bridge could durably record its result remains uncertain and needs reconciliation. Snapshot recovery replays matching calls within a run, not the latest value from unrelated runs.

Revoke an installation from Access → Bridges or `fused-cli bridge revoke <pairing-id>`. Revocation fences the session generation. Inventory withdrawal and offline state block live dispatch immediately; reconnection requires a newly acknowledged inventory. Already accepted events may drain while a profile is offline. Removed profiles retain their pending data without delivery until restored under the same pairing. A device may also repeat self-revocation safely after a lost response. Re-pairing creates a new destination; old backlog must not migrate to it.

## Validation

`BRIDGE_TEST_DATABASE_URL` enables isolated PostgreSQL schemas for Bridge integration tests. The tests never fall back to `DATABASE_URL`. Run `go test -race ./internal/engine/bridgehost` to cover persistence, deduplication, historical manifests, generated gRPC, broker fan-out/outage retention, and cross-replica command uncertainty. `testutil/bridgefixture` supplies a separate loopback fixture for the real Bridge repository, including app publication and restricted family-token execution.

Validated on 2026-10-27: uncached race suites for Bridge transport/storage, Engine API, app store, sandbox, access control, capability hashing, messaging and headless control routes; CLI configuration/API/commands; Registry Bridge-only envelopes; generated TypeScript behavior, Python syntax and complete Go packages; execution runtime tests; UI type checking; static analysis and a headless Engine build. The real Bridge repository also passed its uncached integration run against the disposable Engine fixture, including restricted app-token execution, replay, withdrawal and repeated revocation. No production deployment or live browser OAuth pairing was performed. Generator checks ran directly with local Node 24.21.0; the repository npm gate requires 24.22.1.

Workspace lifecycle validation also covers dynamic profile discovery, empty inventory withdrawal, reconnect deletion reconciliation, stopped-profile command denial, offline backlog drain, removed-profile retention, explicit workspace pairing consent, legacy grant fencing, and control API projection of offline profiles without known schemas.

The extended real-Bridge run verified an added profile was invoked through a real restricted app token without re-pairing, an offline profile rejected its previous pin while retaining its catalogue hash, and profiles deleted during disconnection disappeared after an empty inventory on reconnect. Five repeated Bridge-host race runs and the full Engine API/store/sandbox/control race suites passed. The executable was built; these checks ran the integration fixture, not a replacement of the user's running Engine.
