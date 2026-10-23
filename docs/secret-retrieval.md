# Retrieve API keys and bucket secrets

Fused stores credentials encrypted. API keys (`api_key`) and generic bucket
secrets (`bucket_secret`) can opt into plaintext retrieval with `exportable: true`.
The default is `false`, including existing credentials after schema convergence.
OAuth/OIDC credentials, connected-user tokens, bearer credentials, Basic pairs,
mTLS material, and Fused-managed credentials are not eligible.

Retrieval requires `credentials.reveal` on the exact bucket or its workspace.
`bucket.use`, `credentials.metadata.read`, and `credentials.manage` do not imply
reveal permission. Owner retains all permissions. Other roles, including Admin
and bucket managers, need an explicit custom-role reveal grant. Changing export
policy requires `credentials.manage` and never requires reading the value.

## API

The existing generic-secret `PUT /workspace/buckets/{bucket_id}/secrets` and
API-key `PUT /workspace/secrets` accept `exportable`. API-key rows in the bulk
REST and GraphQL write contracts also accept it. Writes replace the value and
policy together: omitting the flag resets it to `false`. Secret metadata queries
return `id` and `exportable`, without plaintext.

Update an existing eligible secret without replacing its value:

```http
PUT /workspace/buckets/{bucket_id}/secrets/{secret_id}/exportable
Content-Type: application/json

{"exportable":true}
```

Retrieve its current value:

```http
POST /workspace/buckets/{bucket_id}/secrets/{secret_id}/reveal
```

A successful response is `{"value":"..."}` with `Cache-Control: no-store`.
Use a Fused control credential; app execution tokens do not authorize retrieval.
Cookie-authenticated browser requests require the existing CSRF header.
Missing, private, unsupported, or wrong-bucket secrets return the same 404.
Every retrieval checks authoritative storage without using the runtime secret
cache. Disabling exportability prevents subsequent reads; an already admitted
read can finish, and copies already retrieved remain valid until provider rotation.

Retrieval writes attempted and final outcomes through the canonical control
audit, including actor, bucket, opaque secret ID, permission, and request/trace
identity. Plaintext, ciphertext, and secret names never enter these audit records.
Both initial and final audit persistence must succeed before plaintext is sent.
Policy updates record their resulting exportable boolean in the same audit.

## CLI

```sh
# Interactive setup collects values when none are supplied.
fused-cli bucket secret set MY_KEY --bucket default --exportable
fused-cli connect <service> --bucket default --type api_key

# Obtain the opaque secret ID from metadata, then change its policy or reveal it.
fused-cli bucket secret list default --json
fused-cli bucket secret exportable default <secret-id> --enabled=true
fused-cli bucket secret reveal default <secret-id>
fused-cli bucket secret reveal default <secret-id> --json
fused-cli bucket secret exportable default <secret-id> --enabled=false
```

`connect` writes API keys with retrieval disabled. Use `bucket secret exportable`
to opt in after setup or replacement. Generic secret writes accept `--exportable`,
and named retrieval uses `bucket secret get MY_KEY --bucket default`.

`reveal` explicitly prints plaintext. Ordinary output preserves exact bytes with
no appended newline; `--json` returns a `value` field. Listing and writes never
print plaintext. Policy changes require an explicit `--enabled=true|false`.

## UI

The bucket credential composer offers **Allow secret retrieval** for API keys
and generic secrets only. Existing eligible rows offer a **Private/Exportable**
access dialog. Credential managers can save policy; callers with reveal authority
can reveal an opted-in value. Values load only after **Reveal secret**, remain in
local dialog state, and clear on close, window blur, permission-driven unmount,
or after 30 seconds. They are excluded from agent-visible page context.

Once copied, a key can be used outside Fused's operation restrictions and request
audit trail. The retrieval audit records disclosure, not subsequent external use.
