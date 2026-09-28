# Generated SDK with a Unified Operation: local live test

Run on 2026-09-28 against the disposable Fused Registry, Engine, PostgreSQL database, and HTTPS greeting provider. The SDK app is `greeting-unified-sdk-smoke@1.0.0` (version ID `66ec53d4-e8e0-5482-8e76-7c83727a3289`). No production service or credential was used.

## Contract and deployment

The [SDK config](unified-sdk-smoke.yaml) selects one operation, `fused-greeting-sdk-smoke.greet`, from the imported [OpenAPI source](greeting-sdk-smoke.openapi.json). Its `greetings.pair` Unified Operation has two bindings. `first` calls `greet` with the caller's name; `second` depends on `first` and calls `greet` with the first response's `greeting`. A root output projects both strings. The exact input is `{ "name": string }`; the exact output is `{ "first": string, "second": string }`.

The local import and SDK commands were:

```sh
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  import plan execution/examples/greeting-sdk-smoke.openapi.json \
  --name 'Fused Greeting SDK Smoke' --slug fused-greeting-sdk-smoke --strict \
  --receipt-out /private/tmp/fused-greeting-sdk-smoke.import-receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  import apply --receipt /private/tmp/fused-greeting-sdk-smoke.import-receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  workspace service add fused-greeting-sdk-smoke \
  --service-id 781452fa-73a4-4f95-a2d5-4d50f03bf1c4 --version 1.0.0
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  -f execution/examples/unified-sdk-smoke.yaml sdk validate --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  -f execution/examples/unified-sdk-smoke.yaml sdk plan \
  --receipt-out /private/tmp/fused-unified-sdk-smoke.receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  -f execution/examples/unified-sdk-smoke.yaml sdk apply \
  --receipt /private/tmp/fused-unified-sdk-smoke.receipt.json --json
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  sdk download greeting-unified-sdk-smoke@1.0.0 \
  -o /private/tmp/fused-unified-sdk-download --json
npm --prefix /private/tmp/fused-unified-sdk-download/fused-sdks/greeting-unified-sdk-smoke \
  install --offline --no-audit --no-fund
npm --prefix /private/tmp/fused-unified-sdk-download/fused-sdks/greeting-unified-sdk-smoke \
  run build
```

Import plan `5cb0f341-c8b3-4408-8377-e1cdeed7e26f` committed service version `f356f8c6-0057-4400-8dda-efc2ceda6141`. SDK validation returned `valid: true`; SDK plan `fe518477-09a2-49fa-9d86-9254dd1ef8bf` applied, generated, and downloaded the TypeScript package. The SDK's one-time execution token stayed in a protected local file. `npm install` completed in 0.95 seconds and `npm run build` completed in 1.28 seconds.

The disposable Registry process initially lacked its generator files because it ran from a temporary directory. Its generator job completed after that directory received the checked-out `backend/generator` files and a link to the local generator `node_modules`. An earlier SDK apply attempt against a stale Engine-only service snapshot did not commit; the fresh import above supplied Registry's retained generation contract.

The disposable Engine published REST on `127.0.0.1:18081` and gRPC on `127.0.0.1:15051`. It was already running when calls began; this SDK uses Engine gRPC or REST dispatch, so there is no Execution App JavaScript worker startup for these invocations.

## Live requests and results

The shared REST endpoint accepted the SDK's token and Unified request:

```http
POST /v1/apps/66ec53d4-e8e0-5482-8e76-7c83727a3289/executions
Authorization: Bearer $FUSED_SDK_TOKEN
Idempotency-Key: sdk-unified-ada-rest-<unique-id>
Content-Type: application/json

{"operation":"greetings.pair","input":{"name":"AdaRest"},"targets":["first","second"]}
```

It returned HTTP 200 with the **exact transformed output**, `{"first":"Hello AdaRest","second":"Hello Hello AdaRest"}`, in 565.222 ms. The provider saw exactly two calls. The generated TypeScript client then called the same logical operation over gRPC:

```ts
import { FusedSDK } from "greeting-unified-sdk-smoke";

// Share the Engine transport for calls to this immutable SDK version.
const sdk = new FusedSDK({ token: process.env.FUSED_SDK_TOKEN!, grpcUrl: "http://127.0.0.1:15051" });
try {
  const output = await sdk.unified.greetings.pair(
    { name: "AdaSdk" },
    { targets: ["first", "second"] },
  );
  console.log(output);
} finally {
  sdk.close();
}
```

That call returned `{"first":"Hello AdaSdk","second":"Hello Hello AdaSdk"}` in **705.758 ms**, with exactly two provider calls. `sdk activity` recorded it as one successful logical `greetings.pair` execution, receipt `b4817db0-1a78-453f-a8c6-221df1383b5d`.

Three simultaneous calls through one generated SDK instance also succeeded:

| Input name | Output `first` | Output `second` | Caller time |
| --- | --- | --- | ---: |
| `SdkConcurrent1` | `Hello SdkConcurrent1` | `Hello Hello SdkConcurrent1` | 604.959 ms |
| `SdkConcurrent2` | `Hello SdkConcurrent2` | `Hello Hello SdkConcurrent2` | 611.235 ms |
| `SdkConcurrent3` | `Hello SdkConcurrent3` | `Hello Hello SdkConcurrent3` | 602.972 ms |

The mock counted exactly **six** provider operations and a peak of **three** active calls. The separate Dev Execution App worker limit was two during this test; it did not constrain SDK Unified Operations. The account's provider-call limit was four. The three SDK activity receipts were successful with 569–581 ms Engine latency.

An initial `sdk invoke` call executed both provider operations but returned `sdk_execution_response_invalid` while decoding the exact transformed REST output. After correcting the CLI decoder and rebuilding it, the same command succeeded:

```sh
fused-cli --engine-url http://127.0.0.1:18081 --key "$FUSED_TEST_LICENSE" \
  sdk invoke greeting-unified-sdk-smoke@1.0.0 greetings.pair \
  --params '{"name":"AdaCli"}' --target first --target second \
  --token-stdin --json
```

The fixed CLI returned `kind=unified` and `output={"first":"Hello AdaCli","second":"Hello Hello AdaCli"}` with 567.914 ms Engine elapsed time. The mock recorded exactly two calls. Its `output` field is the Engine's exact authored root value; the CLI adds invocation metadata outside it.

## Memory sample for this SDK workload

The same running Engine was sampled through `/proc/1/status` while the disposable provider delayed each physical call by two seconds. Each logical `greetings.pair` call made two sequential provider requests and returned a small JSON object. Engine RSS was **55,052 KiB** idle and **55,052 KiB** at the sampled peak for one active call. With four simultaneous logical calls, it was **55,052 KiB** idle and **55,180 KiB** at peak: **128 KiB total additional resident memory**, or 32 KiB per call if divided evenly. All four calls succeeded, completed in about 4.05 seconds, and made eight provider calls with four active at once.

These RSS deltas show how much the already-warm Go process grew, not all heap bytes allocated and freed by a request. The heap may satisfy calls from previously reserved pages; payload size and selected binding count also affect memory. Before the queue fix, trials of eight and sixteen simultaneous logical calls failed with `output_mapping_failed` after four provider calls were active, so their RSS peaks were excluded from the per-successful-request estimate. A subsequent eight-call diagnostic using `Promise.allSettled` confirmed four successes and four failures: each failed call reported `first: execution_failed` and `second: dependency_failed`. The live Engine entitlement was `max_sandbox_concurrency=4`; at that point SDK Unified physical calls rejected requests beyond the account-wide provider-call limit. The configured final output then could not read the failed target and surfaced `output_mapping_failed`, masking the original concurrency rejection in the brief test script. The provider's normal 250 ms delay was restored afterward.

## Provider queue regression

The Engine was rebuilt from the updated worktree and restarted in the same disposable container:

```sh
GOCACHE=/private/tmp/fused-go-build-cache GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o /private/tmp/fused-engine-unified-queue-linux ./cmd/engine
docker stop fused-execution-test-engine
cp /private/tmp/fused-engine-unified-queue-linux /private/tmp/fused-engine-plan-concurrency
docker start fused-execution-test-engine
```

One generated `FusedSDK` instance then made each batch through `Promise.allSettled`, with unique input names `Queued8-1` through `Queued8-8` and `Queued16-1` through `Queued16-16`:

```ts
const calls = Array.from({ length: count }, (_, index) =>
  sdk.unified.greetings.pair(
    { name: `Queued${count}-${index + 1}` },
    { targets: ["first", "second"] },
  ),
);
const results = await Promise.allSettled(calls);
```

| Concurrent logical calls | Succeeded | Failed | Provider operations | Peak active provider calls | Caller time |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 8 | 8 | 0 | 16 | 4 | 1,279.965 ms |
| 16 | 16 | 0 | 32 | 4 | 2,194.217 ms |

The first eight-call output was `{"first":"Hello Queued8-1","second":"Hello Hello Queued8-1"}`; the first sixteen-call output used `Queued16-1` in the same shape. The account provider limit remained four, and queued SDK Unified children used no extra provider slots. Generated TypeScript and Python output exceptions now include bounded root and target error codes if a target fails. A generator test verifies the message `Unified output could not be produced (output_mapping_failed; first=error:sandbox_concurrency_exceeded, second=skipped:dependency_failed)` without exposing provider bodies. The SDK package deployed for the live success runs predates that generator change; the new message was verified in generator tests.
