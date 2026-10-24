# Unified App snapshots and reruns

Unified Apps can save explicit JSON progress and use it on another attempt of the same logical run. This uses the existing Engine scheduler, PostgreSQL, confined TypeScript executor, credentials, and execution history. It does not require a separate worker service.

```ts
import * as z from "zod/mini";
import { buildUnifiedApp, snapshot } from "@fused/unified-app";

export default buildUnifiedApp({
  input: z.object({}),
  output: z.object({ sum: z.number() }),
  recovery: { mode: "automatic", maxAttempts: 3, backoffSeconds: 5 },
  // Progress is explicit so another interpreter can continue after this attempt ends.
  async execute() {
    // A fresh run starts at the beginning; subsequent attempts load committed progress.
    const saved = (await snapshot.load<{ next: number; sum: number }>()) ?? { next: 1, sum: 0 };
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

`fused.snapshot` exposes the same helpers. `snapshot.save()` resolves only after the Engine commits encrypted progress. It is separate from `fused.db.set()`, whose execution document remains buffered until the terminal result commits. Snapshot data is bounded to 512 KiB and shares the existing host-call and replay-evidence limits.

The function starts again on each attempt. Loading a snapshot does not restore the JavaScript stack, promises, timers, or local variables. If no snapshot exists, the function starts with its original input and `snapshot.load()` returns `null`. A failed or uncertain save cancels the attempt even if authored code catches its error.

## Recovery policy

Newly compiled apps default to `mode: "manual"`, `maxAttempts: 3`, and `backoffSeconds: 5`. An omitted source declaration still produces that explicit manifest policy. Existing published bundles without a recovery declaration retain their earlier behavior; publish a new immutable version to enable snapshots.

- `manual`: another attempt starts only after an authorized rerun request.
- `automatic`: the existing scheduler retries failed, indeterminate, or abandoned attempts after the configured delay or lease expiry.
- `maxAttempts`: 1–10 total attempts, including the initial execution and manually triggered attempts.
- `backoffSeconds`: 1–300 seconds between completed failures and automatic retries.

A completed successful run cannot be rerun as recovery. Submit a new execution to start over with empty progress. Each attempt retains its own execution ID and canonical receipt; all attempts share a `runId`. POST execution responses include `runId` and `attempt`. Attempt history identifies the current attempt; an older execution's result remains immutable.

Automatic recovery requires a direct token belonging to the Unified App family or an Engine-owned cron/webhook trigger. An automatic policy on a delegated SDK/MCP/nested-app invocation is rejected before execution because the original consumer-version authorization cannot be reconstructed from its token ID alone. Manual recovery can use a fresh direct app token and the retained read handle.

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

Recovery preserves the original input, runtime bucket, and immutable app version. Promoting another version blocks execution of the older run; it never interprets an old snapshot using replacement code. Snapshots expire with their retained attempt records under the existing result-retention cleanup.

An external operation can succeed immediately before a crash and before the next snapshot commits. Recovery does not guarantee exactly-once provider effects. Use provider-supported stable idempotency keys or operations that are safe to repeat when enabling automatic recovery.

## Verification

The PostgreSQL tests cover encrypted progress, tenant/version isolation, token revocation, retry bounds, backoff, queued ownership, and concurrent manual/automatic reservations. The compiled TypeScript integration test saves progress, fails, closes its executor, and resumes in a replacement confined process through both the management API and the scheduler. Lost-acknowledgement tests verify that catching a save error cannot turn the attempt into success.

The process test requires an isolated `DATABASE_URL`, the built TypeScript compiler, and an available confined execution worker. It tests authored failure followed by process replacement; it does not claim to kill the whole Engine or to test external provider side effects.

`TestUnifiedAppRecoveryLiveService` additionally calls the public JSONPlaceholder API over HTTPS through the production operation resolver, credential resolver, and dispatcher. For each recovery mode, the compiled app fetches `/todos/1`, commits that response to a snapshot, and deliberately fails. A replacement worker loads the snapshot and fetches `/todos/2`. Assertions decrypt PostgreSQL replay evidence to verify exactly one selected provider operation per attempt, then check that the final output combines both responses. The fixture replaces the runtime cache as well as the worker; provider responses are not mocked. Initial attempts use the production webhook identity builder and scheduled worker entry point, so a missing trigger operation policy cannot be hidden by fixture-created identities.

Run the opt-in service test from the Engine repository with an isolated PostgreSQL URL and a built confined worker:

```sh
FUSED_RECOVERY_LIVE_SERVICE=1 \
DATABASE_URL='<isolated-test-database-url>' \
FUSED_EXECUTION_WORKER_TEST_BINARY='<absolute-worker-binary-path>' \
go test -race ./internal/engine/api -run '^TestUnifiedAppRecoveryLiveService$' -v -count=1
```

This test requires public network access and makes only anonymous GET requests. It does not exercise provider writes, OAuth, or exactly-once side effects. The fixture omits the execution-event publisher, so event-publication warnings are expected and analytics/event delivery is outside its coverage.
