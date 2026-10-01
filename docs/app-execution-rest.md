# REST app execution

The Engine exposes one data-plane REST route for an immutable SDK version or a
stable Unified App family. SDK apps invoke a selected provider operation; Unified Apps invoke
their authored `execute`. The route is shared, but the response shapes differ.

```http
POST /v1/apps/{app_id}/executions
Authorization: Bearer <family-execution-token>
Content-Type: application/json
Idempotency-Key: issue-from-search-42
```

The bearer credential is an app-family execution token, not a workspace API key, control-plane credential, provider token, or MCP session token. SDK callers supply the exact version `app_id`. Unified App callers supply `app_family_id`; Engine resolves the persisted traffic target on each request and validates the family token against that version. Unified App version IDs are rejected with `400 app_family_id_required`. Responses use `Cache-Control: no-store`.

## Request

The body is a strict JSON document. Unknown and duplicate fields are rejected.

```json
{
  "operation": "createIssue",
  "input": {
    "fields": {
      "project": { "key": "ENG" },
      "summary": "Review search result",
      "issuetype": { "name": "Task" }
    }
  },
  "selector": {
    "environment": "production",
    "end_user_ref": "customer-42",
    "auth_type": "oauth",
    "auth_name": "jira",
    "resource_id": "9e751c48-4d16-4eb5-8880-f0f9accd6db4"
  }
}
```

`operation` names one selected physical operation. The Engine returns `409 operation_ambiguous` if multiple physical operations share that name. `input` must be a non-null JSON object; `selector` may contain only the five routing fields shown above. Graph fields such as `targets` and `selectors` are rejected.

The Engine accepts at most 1 MiB of request JSON. Provider credentials and arbitrary secret-shaped selector fields cannot be supplied through this route.

## Idempotency

`Idempotency-Key` is optional for physical execution. Supply it exactly once with at most 256 bytes. Physical replay identity binds the canonical full public request—operation, input, and selector—so a key cannot be reused to change routing. Reuse for different intent returns `409 idempotency_conflict`.

## Responses

A physical response contains one provider JSON value and the provider status code:

```json
{
  "app_id": "cfd33528-5b36-416c-b439-9a1d34cb8860",
  "operation": "createIssue",
  "kind": "physical",
  "status_code": 201,
  "results": [
    { "id": "10042", "key": "ENG-42" }
  ]
}
```

For physical operations selected by a Unified App, `app_family_id` replaces
`app_id` in this response. This SDK app response is a physical operation wrapper. It does not have the
Unified App execution record's `executionId`, `readHandle`, or typed `output`.
This initial REST surface accepts only a single JSON provider response up to
1 MiB. Non-JSON media returns `502 response_not_json`; oversized JSON returns
`502 response_too_large`. Generated SDK and MCP transports retain their existing
media capabilities.

Errors are bounded Engine-owned envelopes. Provider bodies and tokens are never returned:

```json
{
  "error": {
    "code": "connection_required",
    "message": "a provider connection is required",
    "details": {
      "bucket_id": "047bcf05-723c-403d-a14f-b33dc050df66",
      "service_id": "d7346c62-6c74-4fc9-b769-7380f0b6a08d",
      "end_user_ref": "customer-42"
    }
  }
}
```

Actionable connection, reconnect, resource-selection, and environment failures include only safe routing details. Authentication failures are `401`; a valid token without the requested app scope is `403 app_scope_unavailable`; token policy denial is `403 operation_not_allowed`; missing operations are `404 operation_not_found`.

Physical executions publish normal execution receipts with `transport = "rest"`.

## Unified Apps on the same route

A `kind: unified_app` App runs one TypeScript `execute({ input })`. Send its
Zod-validated input to `POST /v1/apps/{app_family_id}/executions`:

```json
{"operation":"execute","input":{"name":"Jane"}}
```

The REST request cannot supply `selector`, `selectors`, `targets`, or
`pagination`; authored code selects its workspace operations and may pass a
page bound to `fused.fetch`. The family URL and execution token stay unchanged
when traffic switches. New requests resolve the promoted version; Engine never
guesses a latest version when the traffic target is absent. Exact version IDs
remain internal execution and control-plane identities.

An accepted execution receives `appFamilyId`, the executed `version`, a durable
`executionId`, `status`, typed
`output`, and a caller-held `readHandle`. Queued or running executions return
HTTP 202; a terminal execution may return HTTP 200 with `status: "failed"`, so
check the recorded status. The stored data document remains searchable but is
not returned in execution responses. It is limited to 512 KiB, and terminal
results are retained for at least 24 hours.

```http
GET /v1/apps/{app_family_id}/executions/{execution_id}
Authorization: Bearer <family-execution-token>
X-Execution-Read-Handle: <handle-from-execute>
```

Search uses `GET /v1/apps/{app_family_id}/executions` with a URL-encoded `where` JSON
object such as `{"data.customerId":"cus_123"}`. It requires an app token
with the `execution:read` grant and accepts only data
paths declared in the compiled App's `fetch.searchable` list. The search page
defaults to 20 results and accepts `limit` from 1 to 100.

`POST /v1/apps/{app_family_id}/executions/{execution_id}/replay` uses the same token
and read handle to run against recorded calls without provider effects.
`POST /v1/apps/{app_family_id}/executions/{execution_id}/rerun` uses the retained
input and calls providers again; it also requires an `Idempotency-Key`.
Both actions create a new execution and require the source to belong to the
current App version. An older source returns `409 app_version_not_current`;
submit a new invocation to run the new contract. Historical reads by execution
ID use the same family URL and handle after promotion. Search uses the current
version and its declared searchable paths.

## Export an OpenAPI document

Export the callable schema for one immutable SDK Version ID from the Engine
control plane:

```http
GET /apps/{app_id}/openapi
X-API-Key: <control-plane-credential>
```

The path accepts the exact SDK `app_id` UUID (shown as **Version ID** by
`fused-cli`), not an SDK name, SDK ID, or a request for the latest version.
The caller needs `app.read` on that SDK. This GET uses the ordinary
Engine control credential; an SDK execution token cannot authorize it.

The response is an OpenAPI 3.1 JSON document for that pinned version's selected
physical operations. It documents only the real
`POST /v1/apps/{app_id}/executions` route, pins `app_id` to the exported Version
ID, and declares the SDK-wide execution token as that POST route's Bearer
credential. The exported document never contains the control credential,
execution-token value, provider credentials, or Registry ingestion evidence.

Use the optional exact operation filter when a consumer needs one operation or
the complete document would exceed the 16 MiB export limit:

```http
GET /apps/{app_id}/openapi?operation=createIssue
X-API-Key: <control-plane-credential>
```

The filter is the exact physical operation name; it does not select
by request shape. A missing operation returns `404`. Because every document is
derived from one immutable app version, export it on demand instead of relying
on a generic checked-in example that could drift from the selected schemas.

### Unified App compilation and host isolation

Inline-source planning type-checks and bundles TypeScript in Node without evaluating authored code. Engine inspects declarations and runs Unified Apps in its OS-confined Goja worker. Local manifest/client generation invokes `fused-engine inspect-unified-app-bundle`. There is no fallback to Node VM or unconfined in-process evaluation.

One persistent worker process serves each app family on each Engine instance. The confined worker compiles its immutable JavaScript bundle once at load time and reuses that program until the version retires. Each invocation receives a fresh Goja runtime, including fresh module state and globals; no runtime objects or host callbacks are cached. Cached executions report zero compilation duration in the existing phase metadata. Up to four invocations run concurrently per family, further limited by the account's `MaxUnifiedAppConcurrency`. A bounded 32-entry admission gate includes running and waiting requests; saturation rejects new requests. Different families run concurrently. Provider/database calls within each invocation are limited to four active calls and 32 total calls. Engine retains authorization and credentials outside the child. Promotion drains the old version before replacing its process. No Docker container is launched per app or per request.

Resident family admission follows the live `MaxUnifiedAppFamilies` account entitlement. Explicit zero denies new families; negative/missing limits remain unlimited. Family startup occurs outside the manager's global mutex so an unrelated family can continue executing. Two compiler pipelines and four transient inspection/direct-execution workers are admitted concurrently. Compiler diagnostics are bounded to 64 KiB. Account limits are not memory reservations; deployment-level memory, CPU, and process limits remain necessary.

On Linux the child uses fresh user, mount, network, PID, IPC, and UTS namespaces, an empty chroot, sanitized environment, and parent-death termination. Resource limits bound its virtual address space to 2 GiB, data segment/anonymous growth to 128 MiB, cumulative process CPU to 300 seconds, and file descriptors to 16. The virtual-address ceiling accommodates Go's reservations and is not a physical-memory reservation. Request execution has a 30-second deadline; declaration inspection has five seconds. A terminated child is replaced on demand. A shared process is still a shared failure domain for its concurrent invocations.

macOS has a parent-applied deny-default `sandbox-exec` profile and Unix resource limits; availability depends on host policy and worker packaging. Native Windows has no implemented equivalent security boundary and rejects Unified App execution. For Windows deployment options and the native implementation requirements, see [Windows Unified App workers](windows-unified-app-workers.md).

`/health` reports `unified_app_worker_ready` from a real isolated worker probe. `FUSED_UNIFIED_APP_WORKER_REQUIRED=true` returns HTTP 503 when that probe fails. Packaged worker presence alone does not prove readiness: namespace restrictions, seccomp, and AppArmor can reject startup. Source compilation also requires Node. MCP continues to use its existing Node/session runtime.

### Engine-wide execution admission

All Unified App invocations (direct, attached SDK/MCP, replay, and rerun) share
one execution budget per Engine server, in addition to existing plan and
per-family limits. The default is `max(1, GOMAXPROCS - 1)` concurrent invocations,
computed when Engine configuration loads. On a two-CPU Engine this normally admits
one invocation at a time, leaving scheduling headroom for control requests.
This limits concurrent work; it is not a dedicated CPU reservation. An invocation
holds its slot while awaiting provider calls too, so I/O-heavy deployments may
need a higher measured limit.

Configure these in `engine.yaml`:

```yaml
engine:
  unified_apps:
    max_concurrency: 1
    queue_capacity: 256
    queue_timeout_seconds: 5
```

Omitted fields use the defaults below. Explicit environment overrides take
precedence over YAML; YAML takes precedence over defaults. These are Engine
operator settings, not per-app source configuration. Restart the Engine after
changing them. The worker Compose example passes through only explicitly set
admission environment variables, preserving mounted YAML settings.

Equivalent environment overrides:

| Setting | Default | Allowed range |
| --- | --- | --- |
| `FUSED_UNIFIED_APP_MAX_CONCURRENCY` | `max(1, GOMAXPROCS - 1)` | 1–1024 |
| `FUSED_UNIFIED_APP_QUEUE_CAPACITY` | 256 waiting requests | 1–65536 |
| `FUSED_UNIFIED_APP_QUEUE_TIMEOUT_SECONDS` | 5 seconds | 1–300 |

Invalid overrides fail startup. Changes require restarting the Engine. Queued
families receive turns in round-robin order; requests within a family remain FIFO.
Each family retains its existing 32-request running-plus-waiting bound. Queue
waits respect caller cancellation and Engine shutdown. A disconnected caller's
running slot is retained until its worker completes or is terminated, preventing
abandoned work from bypassing the budget. Cold worker startup for an invocation
uses the same budget.

Admission failures are retained as terminal execution results with public error
codes `execution_capacity_exceeded` (queue full) or `execution_queue_timeout`
(wait expired). No authored code or provider operation starts for these failures.
Read the execution envelope's status and error code, not HTTP status alone.
Engine does not automatically retry executions. Existing execution receipts and
permission-gated private diagnostics remain the history source.

The Engine-owned OTEL provider exports `engine.unified_app.admission.wait`
(seconds) and `engine.unified_app.admission.requests`, with bounded `outcome`
values `admitted`, `full`, `timeout`, and `cancelled`. The runtime span also carries
`admission.wait_ms` and `admission.outcome`. These measurements exclude payloads,
credentials, and error text. Budgets are per Engine instance, not a distributed
quota across replicas.
