# Unified App snapshots and reruns

Enable `snapshot: true` to checkpoint provider calls automatically. Your function runs normally, without `snapshot.save()` or a resume cursor:

```typescript
import * as z from "zod/mini";
import { buildUnifiedApp } from "@fused/unified-app";
import { fused } from "@fused/operations";

export default buildUnifiedApp({
  snapshot: true,
  recovery: { mode: "automatic", maxAttempts: 3, backoffSeconds: 5 },
  input: z.object({ orderId: z.string() }),
  output: z.object({ ok: z.boolean() }),
  // Select these provider operations in the app configuration before compiling.
  async execute({ input }) {
    await fused.orders.create({ orderId: input.orderId });
    await fused.orders.confirm({ orderId: input.orderId });
    return { ok: true };
  },
});
```

If `create` succeeds and a later call fails, the next attempt starts the function again. Engine returns the saved `create` response without calling the provider again, then continues. The app's original clock and random seed are restored too. Each attempt has a new execution ID; generated provider keys use the stable logical `runId` and call ordinal. Two intentional calls with identical input have distinct keys. Keep call ordering deterministic across retries; a changed request at the same call position is rejected.

Engine uses its existing provider-response store for every keyed call, whether it comes from an SDK, MCP, or a Unified App. It reserves the key before dispatch and acknowledges a successful finite response only after saving it. A repeated key with changed operation, input, token scope, or routing fails instead of returning an unrelated response. Database failures never turn into cache misses.

To supply a business key, pass `idempotencyKey` in the operation options:

```typescript
await fused.orders.create({ orderId }, { idempotencyKey: `order:${orderId}` });
```

The same option is available on `fused.fetch({ service, operation, input, idempotencyKey })`. An explicit key takes precedence over a generated key and works even when `snapshot` is disabled. Keys are scoped to the exact app version. Omitting `snapshot`, or setting it to `false`, generates no automatic provider keys.

Automatic checkpoints currently support finite, non-paginated provider calls with responses up to 16 MiB; existing app response limits still apply. SOAP, streaming contracts, and nested `fused.callApp` calls are rejected before dispatch in this mode. A snapshot-enabled app can itself be invoked through an SDK, MCP server, or another app. Buffered `fused.db` data retains its existing terminal-commit behavior. Manual `snapshot.load()` and `snapshot.save()` cannot be mixed with automatic provider checkpoints. Publish a new immutable version to enable the opt-in.

A provider may commit a write before Engine saves its response. Engine leaves that key pending and refuses to repeat the uncertain write, including after a restart. This prevents a blind duplicate but does **not** provide exactly-once completion or automatic resolution of an unknown outcome. Provider-supported idempotency remains necessary to resolve that crash window safely. Failed contract-declared reads can be retried; uncertain writes require reconciliation. Standalone pending keys are not expired automatically. Completed standalone keys retain their existing 24-hour lifetime; snapshot-bound responses follow the current run's configured retention, including indefinite retention. When runs share an explicit key, Engine keeps one response until every dependent run expires.

## Explicit progress checkpoints

Existing apps can continue using explicit JSON checkpoints with automatic provider snapshots disabled. This mode lets authored code choose where to resume:

```ts
import * as z from "zod/mini";
import { buildUnifiedApp, snapshot } from "@fused/unified-app";

export default buildUnifiedApp({
  input: z.object({}),
  output: z.object({ sum: z.number() }),
  recovery: { mode: "automatic", maxAttempts: 3, backoffSeconds: 5 },
  // Engine restores saved progress before entering this function on every attempt.
  async execute({ input, snapshot: restored }) {
    // Validate the app's JSON state shape; a fresh run begins with null progress.
    const saved = z.nullable(z.object({ next: z.number(), sum: z.number() })).parse(restored)
      ?? { next: 1, sum: 0 };
    let sum = saved.sum;
    // Saving after each bounded unit lets a replacement skip the completed prefix.
    for (let next = saved.next; next <= 5; next++) {
      sum += next;
      await snapshot.save({ next: next + 1, sum });
    }
    return { sum };
  },
});
```

The execution wrapper automatically loads progress before calling `execute({ input, snapshot })`, for initial executions and both manual and automatic recovery. Authors only need to save checkpoints and use the supplied state to continue. The context's `snapshot` is JSON data; validate its shape with Zod as above. Aliasing it as `restored` avoids shadowing the imported `snapshot.save()` helper. The original `input` remains unchanged.

`fused.snapshot.save()` exposes the same save helper. `snapshot.save()` resolves only after the Engine commits encrypted progress. It is separate from `fused.db.set()`, whose execution document remains buffered until the terminal result commits. Snapshot data is bounded to 512 KiB and shares the existing host-call and replay-evidence limits. Automatic restoration uses that recorded host channel too, so diagnostic replay reads the recorded state without accessing live snapshots.

The function starts again on each attempt. Restoring a snapshot does not restore the JavaScript stack, promises, timers, or local variables. If no snapshot exists, `execute` receives `snapshot: null`. A new logical run also starts with null progress. A failed automatic load prevents entry into authored execution; it never falls back to empty progress. A failed or uncertain save cancels the attempt even if authored code catches its error.

Publish a new immutable app version to adopt automatic context restoration; already published bundles keep their compiled behavior. The explicit `snapshot.load()` helper remains available for existing source, but is no longer needed in the authored execution function.

## Recovery policy

Newly compiled apps default to `mode: "manual"`, `maxAttempts: 3`, and `backoffSeconds: 5`. An omitted source declaration still produces that explicit manifest policy. Existing published bundles without a recovery declaration retain their earlier behavior; publish a new immutable version to enable snapshots.

- `manual`: another attempt starts only after an authorized rerun request.
- `automatic`: the existing scheduler retries failed, indeterminate, or abandoned attempts after the configured delay or lease expiry.
- `maxAttempts`: 1–10 total attempts, including the initial execution and manually triggered attempts.
- `backoffSeconds`: 1–300 seconds between completed failures and automatic retries.

A completed successful run cannot be rerun as recovery. Submit a new execution to start over with empty progress. Each attempt retains its own execution ID and canonical receipt; all attempts share a `runId`. POST execution responses include `runId` and `attempt`. Attempt history identifies the current attempt; an older execution's result remains immutable.

Automatic recovery works for direct calls, SDK consumers, MCP servers, nested app calls, schedules and webhooks. Engine retains the exact consumer versions and aliases in the caller chain, without retaining bearer tokens. Each retry and host effect rechecks the original token, its alias grant, applied bindings and current app versions. Revoking the token, removing its alias grant, or retiring any caller or target stops recovery. The synchronous caller receives its attempt’s result; a later automatic attempt runs in Engine and does not replace that response. Manual recovery can use a fresh direct app token and the retained read handle.

## Trigger and inspect a rerun

For a runtime caller, retain the original execution read handle and use the existing family endpoint:

```text
POST /v1/apps/{app_family_id}/executions/{execution_id}/rerun
Authorization: Bearer <family-execution-token>
X-Execution-Read-Handle: <original-read-handle>
Idempotency-Key: <unique-manual-request-key>

GET /v1/apps/{app_family_id}/executions/{execution_id}/attempts
Authorization: Bearer <family-execution-token>
X-Execution-Read-Handle: <original-read-handle>
```

The generated TypeScript client exposes `client.rerun(executionId, readHandle, idempotencyKey)` and `client.attempts(executionId, readHandle)`.

The rerun uses the latest attempt's snapshot, even when the URL names an earlier attempt. Repeating the same idempotency key for that source and token returns the previously accepted attempt. The original read handle also authorizes reads of its recovery attempts. A lost response should be retried with the same key.

App managers can trigger recovery for cron/webhook runs that never issued a caller read handle:

```text
POST /apps/{app_id}/executions/{execution_id}/rerun
Authorization: Bearer <Engine-control-credential>
Idempotency-Key: <unique-manual-request-key>

GET /apps/{app_id}/executions/{execution_id}/attempts
Authorization: Bearer <Engine-control-credential>
```

These management routes use an exact version ID. POST requires the family's `app.unified_app.manage` permission and returns `202` after durable queue admission; the existing scheduler claims the attempt. GET requires `app.unified_app.read` and returns metadata only. The UI has no new recovery controls in this change; these routes are available for clients and later UI integration.

## Ownership and limits

PostgreSQL permits one current attempt per logical run. Revision checks and attempt ownership fence snapshot writes; superseded attempts cannot complete or mutate progress. Concurrent manual and automatic triggers cannot create two owners. The runtime rechecks current deployment and token authority before host effects. Automatic recovery reloads current token grants and binding mode rather than storing bearer credentials.

Recoverable attempts have a maximum 45-second context budget, including admission, and a two-minute ownership lease; existing sandbox limits may be stricter. Abandoned work becomes eligible when its lease expires, then follows the scheduler's polling and bounded batches. Queued management work survives Engine restart and is not mistaken for an already-running attempt. This does not add long-running execution, durable sleep, or autoscaling infrastructure.

Recovery preserves the original input, runtime bucket, and immutable app version. Promoting another version blocks execution of the older run; it never interprets an old snapshot using replacement code. Results, private diagnostics, replay evidence and snapshots share the operator-configured result-retention window.

For explicit progress checkpoints, an external operation can succeed immediately before a crash and before the next snapshot commits. Recovery does not guarantee exactly-once provider effects. Use provider-supported stable idempotency keys or operations that are safe to repeat when enabling automatic recovery. Derive a write key from a stable business operation and item identity, pass the same key on every attempt, then save the snapshot after the provider confirms success. A fresh random key on each attempt defeats deduplication. The provider’s deduplication window must cover the retry window. The `Idempotency-Key` on the rerun endpoint only deduplicates the rerun request; it does not make provider writes exactly-once.

## Result retention

Hosted Engines default to seven days after each attempt completes. Database operators can set any longer period, or use `0` to disable expiry:

```yaml
engine:
  # Keep results, diagnostics, replay history and snapshots until explicitly removed.
  execution_result_retention_days: 0
```

Set this to a positive number of days, such as `90`, for finite retention. Negative values are rejected. Restart Engine after changing the configuration. The setting applies when attempts complete, including interrupted attempts; already-completed records keep their recorded expiry. It is independent of activity-event retention (`execution_retention_days`) and is not capped by a Registry entitlement. An indefinite result omits `expiresAt`. Keeping a result does not restore a retired version's permission to execute.

## Verification

The PostgreSQL tests cover encrypted progress, tenant/version isolation, token revocation, retry bounds, backoff, queued ownership, and concurrent manual/automatic reservations. The compiled TypeScript integration test saves progress, fails, closes its executor, and resumes in a replacement confined process through both the management API and the scheduler. Lost-acknowledgement tests verify that catching a save error cannot turn the attempt into success.

The process test requires an isolated `DATABASE_URL`, the built TypeScript compiler, and an available confined execution worker. It tests authored failure followed by process replacement; it does not claim to kill the whole Engine. `TestAutomaticSnapshotProviderRecovery` additionally uses a local HTTP provider and PostgreSQL: one write succeeds, the next read fails, and a replacement worker completes with one provider write total. Reservation tests cover concurrent callers and lost persistence acknowledgements. Retention tests expire the original attempt while preserving responses needed by the current attempt.

`TestUnifiedAppRecoveryLiveService` additionally calls the public JSONPlaceholder API over HTTPS through the production operation resolver, credential resolver, and dispatcher. For each recovery mode, the compiled app fetches `/todos/1`, commits that response to a snapshot, and deliberately fails. A replacement worker loads the snapshot and fetches `/todos/2`. Assertions decrypt PostgreSQL replay evidence to verify exactly one selected provider operation per attempt, then check that the final output combines both responses. The fixture replaces the runtime cache as well as the worker; provider responses are not mocked. Initial attempts use the production webhook identity builder and scheduled worker entry point, so a missing trigger operation policy cannot be hidden by fixture-created identities.

Run the opt-in service test from the Engine repository with an isolated PostgreSQL URL and a built confined worker:

```sh
FUSED_RECOVERY_LIVE_SERVICE=1 \
DATABASE_URL='<isolated-test-database-url>' \
FUSED_EXECUTION_WORKER_TEST_BINARY='<absolute-worker-binary-path>' \
go test -race ./internal/engine/api -run '^TestUnifiedAppRecoveryLiveService$' -v -count=1
```

This test requires public network access and makes only anonymous GET requests. It does not exercise provider writes, OAuth, or exactly-once side effects. The fixture omits the execution-event publisher, so event-publication warnings are expected and analytics/event delivery is outside its coverage.
