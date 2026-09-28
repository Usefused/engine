# Execution App local smoke evidence

The resident-worker validation at the end of this record is the current implementation. Earlier per-invocation timing and two-call limit observations describe the implementation before the resident worker and bounded queue changes.

Run on 2026-09-28 against a disposable local Registry, PostgreSQL database, HTTPS mock provider, and Engine Docker container. No remote Engine or production Registry was changed. The test provider had one imported, workspace-enabled `POST /greet` operation. Its certificate was validated against a one-day test CA mounted only into the disposable Engine. The mock counted valid requests without retaining their bodies.

## Authored contract and bundle

The hosted source used the selected-operation methods generated from the build spec:

```ts
import * as z from "zod/mini";
import { buildExecutionApp, fused } from "@fused/execution";
import { services } from "@fused/operations";

export default buildExecutionApp({
  input: z.object({ name: z.string() }),
  output: z.object({ greeting: z.string() }),
  fetch: { searchable: ["name"] },
  // The selected workspace operation supplies output; stored data supports fetch search.
  async execute({ input }) {
    const result = await services["fused-greeting-smoke"].greet({ name: input.name });
    await fused.db.set({ name: input.name });
    return { greeting: result.greeting };
  },
});
```

The local build spec selected service `bd7c73e9-d4a2-4936-9d01-0029127bff9d`, version `21d0f322-bb32-407c-9bfb-431288c23a89`, and endpoint `3957083b-0692-410a-a479-b83beb99564a` as `fused-greeting-smoke.greet`. Reviewed request/response JSON Schemas typed the generated method and were absent from the admitted manifest.

```sh
cd execution
npm test
node dist/src/cli.js \
  --config /private/tmp/fused-live-service.spec.json \
  --out /private/tmp/fused-live-service-small.bundle.js \
  --manifest /private/tmp/fused-live-service-small.manifest.json \
  --client /private/tmp/fused-live-service-small.client.ts \
  --bindings /private/tmp/fused-live-service-small.operations.d.ts
```

`npm test`: 12/12 passed, complexity ≤10. The optimized self-contained bundle was **27,810 bytes** (9,374 bytes gzip), down from 755,011 bytes before ESM tree shaking and Zod mini authoring. Its digest was `sha256:6ef8060c7d81e5902dfcce2c7e364cbde50d61526bc98ae6cbcdc66537e47867`. The public manifest was identical before and after optimization.

## Disposable deployment

The app config used `kind: execution`, version `1.0.2`, `bundle_digest` above, bucket `default`, and selected `fused-greeting-smoke` version `1.0.0` operation `greet`. The test license and runtime token were supplied from local protected files and are omitted here.

```sh
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  -f /private/tmp/fused-live-service-app-v102.yaml execution plan \
  --receipt-out /private/tmp/fused-live-service-app-v102.receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  -f /private/tmp/fused-live-service-app-v102.yaml execution apply \
  --receipt /private/tmp/fused-live-service-app-v102.receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  execution bundle attach 067b4528-8da9-55b0-88b5-133310ce0eca \
  --source-hash sha256:1e4c62b2a0568e0bd04d06bd6da115f0925131a8e18dcb25bcbd703a0abaa021 \
  --bundle /private/tmp/fused-live-service-small.bundle.js \
  --manifest /private/tmp/fused-live-service-small.manifest.json --json
```

Plan `b6aeaf15-06ce-4588-be41-a0e162d02c6e` succeeded; apply created app version `067b4528-8da9-55b0-88b5-133310ce0eca`; attach returned `{"status":"attached"}`. The plan required `app.execution.create`, `bucket.use`, and `service.consume`.

## Hosted calls

The existing REST route was used for every invocation. `$FUSED_APP_TOKEN` is a disposable family token; `$READ_HANDLE` is the per-execution caller-held handle. Neither value appears in this file.

```sh
APP_ID=067b4528-8da9-55b0-88b5-133310ce0eca
BASE=http://127.0.0.1:18081/v1/apps/$APP_ID/executions
EXECUTION_ID=190a7031-febe-46a4-8bd5-a9c72874c40d
curl -H "Authorization: Bearer $FUSED_APP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"operation":"execute","input":{"name":"Ada"}}' "$BASE"
curl -H "Authorization: Bearer $FUSED_APP_TOKEN" \
  -H "X-Execution-Read-Handle: $READ_HANDLE" "$BASE/$EXECUTION_ID"
curl -G -H "Authorization: Bearer $FUSED_APP_TOKEN" \
  --data-urlencode 'where={"data.name":"Ada"}' "$BASE"
curl -X POST -H "Authorization: Bearer $FUSED_APP_TOKEN" \
  -H "X-Execution-Read-Handle: $READ_HANDLE" "$BASE/$EXECUTION_ID/replay"
curl -X POST -H "Authorization: Bearer $FUSED_APP_TOKEN" \
  -H "X-Execution-Read-Handle: $READ_HANDLE" \
  -H 'Idempotency-Key: greeting-small-rerun-v9' "$BASE/$EXECUTION_ID/rerun"
```

| Call | Result |
| --- | --- |
| Execute `Ada` | HTTP 200, execution `190a7031-febe-46a4-8bd5-a9c72874c40d`, `succeeded`, output `{"greeting":"Hello Ada"}`, data `{"name":"Ada"}` |
| Fetch | HTTP 200, same output and data |
| Search `data.name=Ada` | HTTP 200, four matching items across the original execute, two replays, and rerun |
| Replay with provider stopped | HTTP 200, execution `375c853f-722d-4ed1-941f-468f1515a2b9`, `succeeded`, `mode=replay`, original source ID, same output and data |
| Rerun with provider running | HTTP 200, execution `1a708f9e-40cf-476f-b41b-72000fc63a68`, `succeeded`, `mode=rerun`, original source ID, same output and data |
| Fresh execute `Grace` on the extracted worker | HTTP 200, execution `f7c91fd1-f245-48e0-9b79-cba4967afaa8`, `succeeded`, output `{"greeting":"Hello Grace"}`, data `{"name":"Grace"}`; mock provider hits increased from 0 to 1 |
| Fresh execute `Lin` under the final arm64 seccomp profile | HTTP 200, execution `b7089f43-9971-4c05-a4ba-5940b341642a`, `succeeded`, output `{"greeting":"Hello Lin"}`, data `{"name":"Lin"}`; mock provider hits increased from 1 to 2 |

## Multiple requests

The restored, unmodified Engine build handled two simultaneous POST requests to the same Execution App. `FinalConcurrent1` produced execution `7df5a935-ad9a-4f9b-af90-25eeae061dd0` and `{"greeting":"Hello FinalConcurrent1"}`; `FinalConcurrent2` produced execution `e010a973-58f2-4638-b912-7d973dd09d85` and `{"greeting":"Hello FinalConcurrent2"}`. Both returned HTTP 200 with `status=succeeded`. Separate GET requests using each caller's read handle returned the matching saved output, and the mock provider counter increased by two.

Five simultaneous requests were also tried. The disposable Engine's stored entitlement was `plan=dev`, `max_sandbox_concurrency=2`, so two succeeded and three recorded `status=failed`. The diagnostic dispatcher error for those three was `sandbox_concurrency limit reached (2/2)`. The limit applies to concurrent physical provider calls across the account, not to the number of deployed Execution Apps.

## Per-invocation startup timing

The Engine stays running; each invocation starts a fresh isolated worker. Temporary timing added to the disposable Engine measured 12 sequential invocations of this 27,810-byte bundle. Median OS process launch was **2.9 ms**. Median time from launch to the worker's first provider-call request, including sandbox and JavaScript initialization, was **12.0 ms**. The first worker after Engine restart took 25.2 ms to reach that point; the other 11 took 10.2–13.4 ms. Full caller-observed requests took 28.8–46.0 ms after the first request; the first took 191.0 ms while Engine loaded its service runtime cache. All 12 succeeded. The temporary timing code was removed and the original Engine build restored afterward.

The final disposable Engine used a separate stripped Linux arm64 `fused-execution-worker` binary of **10,223,776 bytes**. Its OS-confined worker ran with 2 GiB virtual address and 128 MiB data limits. Real declaration inspection of the original 755,011-byte bundle peaked at 27,756 KiB RSS; inspection of the optimized 27,810-byte bundle peaked at 10,652 KiB RSS.

## Final arm64 seccomp profile

The final test replaced the earlier permissive Docker setting with [the arm64 worker seccomp profile](../../fused-provisioner/deploy/seccomp/fused-execution-worker-arm64.json). The disposable Engine ran as UID/GID `100:101`, with all Linux capabilities dropped, no new privileges, and `FUSED_EXECUTION_APP_WORKER_REQUIRED=true`. `FUSED_SECCOMP_PROFILE` denotes that profile's absolute checkout path. This is the relevant Docker invocation; the protected env file supplied the disposable license, encryption key, and database URL:

```sh
docker run -d --name fused-execution-test-engine --network fused-execution-test-net \
  --user 100:101 --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp="$FUSED_SECCOMP_PROFILE" \
  -p 127.0.0.1:18081:8081 --env-file /private/tmp/fused-execution-engine.env \
  -e SSL_CERT_FILE=/etc/ssl/certs/fused-greeting-ca.crt \
  -e FUSED_EXECUTION_APP_WORKER_REQUIRED=true \
  -v /private/tmp/fused-engine-live-v9:/app/fused-engine:ro \
  -v /private/tmp/fused-execution-worker-v9:/app/fused-execution-worker:ro \
  -v /private/tmp/fused-greeting-ca.crt:/etc/ssl/certs/fused-greeting-ca.crt:ro \
  --entrypoint /bin/sh fused-engine-sdk-init-live:local \
  -c 'exec /app/fused-engine start --license-key "$FUSED_LICENSE_KEY" --config /tmp/fused-execution-empty-config.yaml --port 8081 --grpc-host 127.0.0.1'
```

`GET /health` returned HTTP 200. A fresh `POST /v1/apps/067b4528-8da9-55b0-88b5-133310ce0eca/executions` with `{"operation":"execute","input":{"name":"Lin"}}` returned HTTP 200 and the result shown above. The valid provider-call counter increased by one. The container remained on the disposable Docker network; no production isolation path was relaxed.

## Resident worker live validation

On 2026-09-28, the same Zod-authored TypeScript app and 27,810-byte bundle above ran against the patched Engine. The selected operation was `fused-greeting-smoke.greet` (`POST /greet` on the disposable HTTPS provider). Every `execute` called that operation once, returned its greeting, and stored `{"name":input.name}` through `fused.db`. The hosted immutable Execution App version was `067b4528-8da9-55b0-88b5-133310ce0eca`.

The TypeScript compilation command was:

```sh
cd execution
npm test
node dist/src/cli.js \
  --config /private/tmp/fused-live-service.spec.json \
  --out /private/tmp/fused-live-service-small.bundle.js \
  --manifest /private/tmp/fused-live-service-small.manifest.json \
  --client /private/tmp/fused-live-service-small.client.ts \
  --bindings /private/tmp/fused-live-service-small.operations.d.ts
```

The final Engine and confined worker were built and hosted locally with the following commands. `FUSED_ENGINE_WORKTREE` denotes the Engine feature worktree; `FUSED_SECCOMP_PROFILE` denotes the linked profile's absolute checkout path.

```sh
cd "$FUSED_ENGINE_WORKTREE"
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags headless -ldflags '-s -w' \
  -o /private/tmp/fused-engine-resident-final ./cmd/engine
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags headless -ldflags '-s -w' \
  -o /private/tmp/fused-execution-worker-resident-live ./cmd/execution-worker
docker run -d --name fused-execution-test-engine --network fused-execution-test-net \
  --user 100:101 --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp="$FUSED_SECCOMP_PROFILE" \
  -p 127.0.0.1:18081:8081 --env-file /private/tmp/fused-execution-engine.env \
  -e SSL_CERT_FILE=/etc/ssl/certs/fused-greeting-ca.crt \
  -e FUSED_EXECUTION_APP_WORKER_REQUIRED=true \
  -v /private/tmp/fused-engine-resident-final:/app/fused-engine:ro \
  -v /private/tmp/fused-execution-worker-resident-live:/app/fused-execution-worker:ro \
  -v /private/tmp/fused-greeting-ca.crt:/etc/ssl/certs/fused-greeting-ca.crt:ro \
  --entrypoint /bin/sh fused-engine-sdk-init-live:local \
  -c 'exec /app/fused-engine start --license-key "$FUSED_LICENSE_KEY" --config /tmp/fused-execution-empty-config.yaml --port 8081 --grpc-host 127.0.0.1'
```

The local Registry supplied a fresh disposable license for the existing app account. No token or read handle is included in this record. The API request shape was:

```sh
curl -H "Authorization: Bearer $FUSED_APP_TOKEN" -H 'Content-Type: application/json' \
  -d '{"operation":"execute","input":{"name":"FinalVerified1"}}' \
  http://127.0.0.1:18081/v1/apps/067b4528-8da9-55b0-88b5-133310ce0eca/executions
```

The `FinalVerified1` response was HTTP 200, `status=succeeded`, execution ID `730809a1-9fe8-4242-a7b7-1fa08cbfd34c`, output `{"greeting":"Hello FinalVerified1"}`, and stored data `{"name":"FinalVerified1"}`. A GET of that execution with its caller-held read handle returned the same output. The five requests below began concurrently on the final refactored binary; every saved result was fetched with HTTP 200.

| Input name | Execution ID | Output greeting | Caller time | Created-to-completed time |
| --- | --- | --- | ---: | ---: |
| `FinalVerified1` | `730809a1-9fe8-4242-a7b7-1fa08cbfd34c` | `Hello FinalVerified1` | 247.327 ms | 186.495 ms |
| `FinalVerified2` | `d43a9f96-5e06-4bac-88d5-6e5ad9378fb6` | `Hello FinalVerified2` | 246.550 ms | 182.298 ms |
| `FinalVerified3` | `64006723-6b6f-417c-b525-cea11726b09e` | `Hello FinalVerified3` | 242.333 ms | 156.432 ms |
| `FinalVerified4` | `58adc613-190b-47de-8978-73d1040e6761` | `Hello FinalVerified4` | 253.513 ms | 173.061 ms |
| `FinalVerified5` | `abc932ab-7a06-49ff-b668-8a8f17b9b5d4` | `Hello FinalVerified5` | 243.253 ms | 178.415 ms |

Provider hits rose from 58 to 63: exactly one selected physical operation per execution. On Scale-up the final Engine prewarmed this app version **before any request** in 24 ms, logged one worker PID (`26` inside the container), and the concurrent batch added no worker-start log entry. The broad Go suite was running concurrently, so these caller times are not an isolated latency benchmark. An earlier patched build prewarmed in 23 ms, stayed resident after the 30-second Dev idle window, and served `ScaleUpReuse` in 87.071 ms with output `{"greeting":"Hello ScaleUpReuse"}` without starting another worker.

On Dev, five concurrent requests to the same app also all succeeded despite the account's two-physical-call limit. The bounded provider-call queue admitted them; each had a distinct execution ID and matching fetched output, with caller times 82.821–95.800 ms and created-to-completed times 53.782–68.092 ms. The mock counted exactly five calls. A separate patched-build `IdleProbe` returned HTTP 200 with output `{"greeting":"Hello IdleProbe"}` in 181.012 ms; its worker startup was 27 ms. The process then retired after 30 seconds idle while Engine `/health` remained HTTP 200. The idle test found and fixed a timer reset caused by periodic entitlement reconciliation.

The first resident-worker request failed because the disposable mock provider had stopped; restarting the provider made the same request succeed. The local Engine had no OTEL exporter configured, so startup measurements came from structured Engine logs. The code also emits worker startup, run, and queue-wait spans when an exporter is configured.

## Plan-configured Dev app concurrency

The same compiled TypeScript bundle and hosted app above were tested on 2026-09-28 after adding `max_execution_app_concurrency` to the plan and Engine entitlement. The local Registry used a disposable Dev plan with `max_execution_app_concurrency: 2` and `max_sandbox_concurrency: 4`; the second value was raised from the normal Dev value of 2 only to isolate the app limit from the account-wide provider-call limit. The Engine database confirmed `dev|2|4`. The Engine was rebuilt for Linux arm64 and started with the same confined worker and Docker settings documented above:

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -tags headless -ldflags '-s -w' \
  -o /private/tmp/fused-engine-plan-concurrency ./cmd/engine
docker run -d --name fused-execution-test-engine --network fused-execution-test-net \
  --user 100:101 --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp="$FUSED_SECCOMP_PROFILE" \
  -p 127.0.0.1:18081:8081 --env-file /private/tmp/fused-execution-engine.env \
  -e SSL_CERT_FILE=/etc/ssl/certs/fused-greeting-ca.crt \
  -e FUSED_EXECUTION_APP_WORKER_REQUIRED=true \
  -v /private/tmp/fused-engine-plan-concurrency:/app/fused-engine:ro \
  -v /private/tmp/fused-execution-worker-resident-live:/app/fused-execution-worker:ro \
  -v /private/tmp/fused-greeting-ca.crt:/etc/ssl/certs/fused-greeting-ca.crt:ro \
  --entrypoint /bin/sh fused-engine-sdk-init-live:local \
  -c 'exec /app/fused-engine start --license-key "$FUSED_LICENSE_KEY" --config /tmp/fused-execution-empty-config.yaml --port 8081 --grpc-host 127.0.0.1'
```

Five concurrent `POST /v1/apps/067b4528-8da9-55b0-88b5-133310ce0eca/executions` requests used `{"operation":"execute","input":{"name":"PlanDevN"}}` for `N=1..5`. Each execution made one `fused-greeting-smoke.greet` operation and stored `{"name":"PlanDevN"}`. The mock provider delayed each response by 250 ms and recorded its active request count. All five requests returned HTTP 200 and `status=succeeded`; separate authenticated GETs by execution ID and private read handle returned the matching output and stored data.

| Input | Execution ID | Output | Caller time | Created-to-completed |
| --- | --- | --- | ---: | ---: |
| `PlanDev1` | `444625f1-e559-4676-a330-51414110bfb7` | `Hello PlanDev1` | 1018.947 ms | 983.287 ms |
| `PlanDev2` | `b36c4f5d-afbc-478e-8287-b81ec9235828` | `Hello PlanDev2` | 475.288 ms | 434.768 ms |
| `PlanDev3` | `546e0bef-9ff7-4bd1-b459-87f10c58ff56` | `Hello PlanDev3` | 744.197 ms | 711.052 ms |
| `PlanDev4` | `8480bcbf-7e78-4078-8f61-f21d79a89423` | `Hello PlanDev4` | 747.390 ms | 703.876 ms |
| `PlanDev5` | `c10f5ef9-0ef2-4fdd-bb21-c0f139642ccb` | `Hello PlanDev5` | 475.649 ms | 438.576 ms |

The provider received exactly five calls, and its maximum observed overlap was **2** while the account provider-call limit was **4**. The Engine logged one resident worker load, PID 27 inside the container, with **24 ms startup**; the batch did not start one worker per request. The local Engine had no OTEL exporter configured, so its startup log and the provider's active-call trace supplied the measurements. A separate PostgreSQL integration test, `go test ./internal/engine/store -run '^TestRuntimeEntitlementRoundTrip$' -count=1 -v`, passed against a disposable database and confirmed persistence of the new limit.
