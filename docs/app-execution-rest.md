# REST app execution

The Engine exposes one data-plane REST route for an immutable SDK or Unified
App version. SDK apps invoke a selected provider operation; Unified Apps invoke
their authored `execute`. The route is shared, but the response shapes differ.

```http
POST /v1/apps/{app_id}/executions
Authorization: Bearer <family-execution-token>
Content-Type: application/json
Idempotency-Key: issue-from-search-42
```

The bearer credential is an app-family execution token, not a workspace API key, control-plane credential, provider token, or MCP session token. The Engine validates the token against the exact `app_id` in the path. Responses use `Cache-Control: no-store`.

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

This SDK app response is a physical operation wrapper. It does not have the
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
Zod-validated input using the same POST path:

```json
{"operation":"execute","input":{"name":"Jane"}}
```

The REST request cannot supply `selector`, `selectors`, `targets`, or
`pagination`; authored code selects its workspace operations and may pass a
page bound to `fused.fetch`. Only the current App version in a family accepts
new traffic. A prior version returns `409 app_version_not_current`, while its
retained results remain readable until expiration.

An accepted execution receives a durable `executionId`, `status`, typed
`output`, and a caller-held `readHandle`. Queued or running executions return
HTTP 202; a terminal execution may return HTTP 200 with `status: "failed"`, so
check the recorded status. The stored data document remains searchable but is
not returned in execution responses. It is limited to 512 KiB, and terminal
results are retained for at least 24 hours.

```http
GET /v1/apps/{app_id}/executions/{execution_id}
Authorization: Bearer <family-execution-token>
X-Execution-Read-Handle: <handle-from-execute>
```

Search uses `GET /v1/apps/{app_id}/executions` with a URL-encoded `where` JSON
object such as `{"data.customerId":"cus_123"}`. It requires an app token
with the `execution:read` grant and accepts only data
paths declared in the compiled App's `fetch.searchable` list. The search page
defaults to 20 results and accepts `limit` from 1 to 100.

`POST /v1/apps/{app_id}/executions/{execution_id}/replay` uses the same token
and read handle to run against recorded calls without provider effects.
`POST /v1/apps/{app_id}/executions/{execution_id}/rerun` uses the retained
input and calls providers again; it also requires an `Idempotency-Key`.
Both actions create a new execution and require the current App version.

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
