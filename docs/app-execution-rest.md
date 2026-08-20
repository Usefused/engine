# REST app execution

The Engine exposes one data-plane REST route for an immutable SDK app:

```http
POST /v1/apps/{app_id}/executions
Authorization: Bearer <family-execution-token>
Content-Type: application/json
Idempotency-Key: issue-from-search-42
```

The bearer credential is an app-family execution token, not a workspace API key, control-plane credential, provider token, or MCP session token. The Engine validates the token against the exact `app_id` in the path and accepts only SDK app runtimes. Responses use `Cache-Control: no-store`.

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

`operation` is matched exactly. The Engine infers `physical` or `unified` from the app's immutable runtime definitions; the name's syntax and request shape never choose the kind. If the name resolves to more than one physical operation, or to both a physical and Unified operation, the Engine returns `409 operation_ambiguous` instead of guessing.

For a physical operation, `input` must be a non-null JSON object and `selector` may contain only the five routing fields shown above. `targets` and `selectors` are rejected. For a Unified operation, `input` may be any JSON value and `targets` must contain 1–16 explicit, unique targets from the declared graph. Unified execution never defaults to every target. The optional `selectors` map applies the same safe routing fields to its target-keyed entries:

```json
{
  "operation": "searchAndNotify",
  "input": "OpenAPI parser improvements",
  "targets": ["nimble", "plunk"],
  "selectors": {
    "nimble": { "auth_name": "default" },
    "plunk": { "auth_name": "transactional" }
  }
}
```

The Engine accepts at most 1 MiB of request JSON. Provider credentials and arbitrary secret-shaped selector fields cannot be supplied through this route.

## Idempotency

`Idempotency-Key` is required for Unified execution and optional for physical execution. Supply it exactly once with at most 256 bytes. Physical replay identity binds the canonical full public request—operation, input, and selector—so a key cannot be reused to change routing. Reuse for different intent returns `409 idempotency_conflict`.

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

This initial REST surface accepts only a single JSON provider response up to 1 MiB. Non-JSON media returns `502 response_not_json`; oversized JSON returns `502 response_too_large`. Generated SDK and MCP transports retain their existing media capabilities.

A Unified response preserves target order and always includes `rollbacks`, even when empty:

```json
{
  "app_id": "cfd33528-5b36-416c-b439-9a1d34cb8860",
  "operation": "searchAndNotify",
  "kind": "unified",
  "results": [
    { "target": "nimble", "status": "success", "data": { "answer": "..." } },
    { "target": "plunk", "status": "success", "data": null }
  ],
  "rollbacks": []
}
```

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

Actionable connection, reconnect, resource-selection, and environment failures include only safe routing details. Authentication failures are `401`; a valid token without the requested SDK app scope is `403 app_scope_unavailable`; token policy denial is `403 operation_not_allowed`; missing operations are `404 operation_not_found`.

Physical executions and every Unified child publish normal execution receipts with `transport = "rest"`. Unified orchestration does not publish a duplicate wrapper receipt.

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

The response is an OpenAPI 3.1 JSON document for that pinned version's physical
and Unified operations. It documents only the real
`POST /v1/apps/{app_id}/executions` route, pins `app_id` to the exported Version
ID, and declares the SDK-wide execution token as that POST route's Bearer
credential. The exported document never contains the control credential,
execution-token value, provider credentials, private Unified mappings, or
Registry ingestion evidence.

Use the optional exact operation filter when a consumer needs one operation or
the complete document would exceed the 16 MiB export limit:

```http
GET /apps/{app_id}/openapi?operation=createIssue
X-API-Key: <control-plane-credential>
```

The filter is the exact physical or Unified operation name; it does not select
by request shape. A missing operation returns `404`. Because every document is
derived from one immutable app version, export it on demand instead of relying
on a generic checked-in example that could drift from the selected schemas.
