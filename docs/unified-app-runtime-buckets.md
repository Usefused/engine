# Unified App runtime buckets

Runtime bucket selection is disabled by default. An immutable Unified App version opts in by declaring a finite allowlist of existing bucket names:

```yaml
apiVersion: fused/v1
kind: unified_app
name: customer-lookup
version: "1.1.0"
bucket: default
runtime_buckets:
  - tenant-a
  - tenant-b
source_path: customer-lookup.ts
services:
  crm:
    version: "2026-01-01"
    operations: [getCustomer]
```

Omit `runtime_buckets`, or use `[]`, to disable selection. Up to 32 distinct names are allowed; wildcards are rejected. Adding or removing permitted buckets requires a new version. Plan resolves names to immutable bucket IDs and includes `bucket.use` requirements for every permitted bucket in the normal actor/owner authorization. Apply rechecks the name-to-ID mapping, so deleting and recreating a bucket cannot inherit a reviewed plan's authority.

## API

Invoke the stable Unified App family URL with its execution token. `bucket_id` is an Engine control outside the authored `input`:

```http
POST /v1/apps/<app-family-id>/executions
Authorization: Bearer <execution-token>
Content-Type: application/json

{
  "operation": "execute",
  "bucket_id": "11111111-1111-4111-8111-111111111111",
  "input": {"customerId": "cus_123"}
}
```

An explicit selection replaces both the default and per-service bucket routing for every physical provider call in that execution, including connected-user credential lookup and bucket values. Without a selection, existing configured routing remains in effect. The Engine neither changes the family binding nor exposes credentials to app source. Missing credentials fail in the selected bucket; they do not fall back to another bucket.

Every selected execution checks the deployed allowlist, current workspace/account and traffic target, live token validity and `execute` authority, and the owning subject/team's current `bucket.use` grant. The same uncached check runs again before resolving provider credentials. Execution-token possession delegates the app owner's authority within this finite scope; it does not grant access to arbitrary workspace buckets. Existing dynamic family tokens permitting `execute` can use newly deployed permitted buckets. Fixed connection tokens cannot select a runtime bucket.

Denials return `403 bucket_selection_denied`; authorization-storage failures fail closed with `503 runtime_unavailable`. Unsupported raw-operation or attachment requests return `400 bucket_selection_unsupported`. Explicit null, empty or zero `bucket_id` and `expected_app_id` values return `400 invalid_request`; omit these fields to use configured routing without a version fence. Authorization occurs before durable acceptance, including `Prefer: respond-async` requests.

The execution response and retained result include `bucketId` when explicitly selected. Reruns retain that ID and reauthorize it; idempotency also checks routing identity. Replays retain the ID, reauthorize access, and use recorded effects. Deleting a bucket does not erase historical routing identity. Nested app calls do not inherit the caller's override. Hosted MCP, attachments, schedules and webhooks continue using their own static configuration; this selector belongs to direct Unified App REST execution.

The management OpenAPI export advertises `bucket_id` only for opted-in versions, with an enum of permitted IDs and `x-fused-bucket-names` display labels. Those labels describe deployed scope; current authorization remains authoritative.

## CLI and generated client

```sh
fused-cli app plan -f .fused/unified_app/customer-lookup.yaml
fused-cli app apply -f .fused/unified_app/customer-lookup.yaml
fused-cli app invoke customer-lookup@1.1.0 execute \
  --params '{"customerId":"cus_123"}' \
  --bucket-id 11111111-1111-4111-8111-111111111111 \
  --token-env FUSED_APP_TOKEN
```

`--bucket-id` accepts a nonzero UUID only on Unified App `execute`. The CLI invokes the stable family route with an `expected_app_id` version fence; promotion between selection and dispatch returns a conflict instead of executing different code. API callers can send the same optional field. Ordinary invocations without the flag keep static routing.

The generated TypeScript execution client supports:

```ts
await client.execute({customerId: "cus_123"}, {bucketId: selectedBucketId});
```

## UI

In Unified App setup, choose **Choose at runtime** in the **Bucket** dropdown, select permitted buckets, and retain a default bucket for calls without a selection. The form and YAML editor preserve the same scope and invalidate any earlier plan when it changes. Existing apps require a new version.

On an opted-in version's overview, expand **Run with a runtime bucket**, select a permitted bucket, and enter values directly in the input chips supplied by the app's schema. Required fields are marked; optional fields remain omitted until filled. Changes apply as you type, with schema errors and incomplete values blocking execution. Use **JSON** for advanced input or schemas that cannot be represented as fields. Supply an execution token, then run. The UI also copies the complete request JSON. Tokens remain local to the component, are cleared after the request, and are not saved. Execution is never retried automatically.
