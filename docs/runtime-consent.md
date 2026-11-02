# Runtime consent for API apps

Minion and other infrastructure clients can provision an ordinary Fused API app through a browser approval ceremony. This uses the existing `kind: sdk`, `generate: false` app lifecycle; it does not introduce a new app kind or generate a package. The resulting app uses the same Engine gRPC execution protocol as generated SDKs.

## Execution-only wire contract

1. `POST /oauth/runtime/register` with JSON `{ "name": "Support", "redirect_uri": "http://127.0.0.1:1234/callback", "manifest": "<exact JSON bytes as a string>" }`.
2. Preserve the returned `client_id`, one-time `client_secret`, `client_id_expires_at`, and `manifest_digest`. The unapproved registration expires after one hour.
3. Open `/oauth/runtime/authorize` with `client_id`, `redirect_uri`, `response_type=code`, `scope=api.execute`, unpredictable `state`, and PKCE `code_challenge`/`code_challenge_method=S256`.
4. The signed-in owner reviews the compiled operations, reviews a caller-selected `bucket` (exact name or canonical UUID) read-only, or uses the searchable picker for a default and optional additional buckets when `bucket` is omitted, and approves. Unavailable caller-selected buckets fail closed; form fields cannot substitute or expand that fixed routing. The existing app planner/apply validates ownership, service access, bucket access, immutable versions, and quota. Token-management authority is required for the resulting API family. The registered manifest cannot be replaced by browser form fields.
5. Exchange the code at `POST /oauth/runtime/token` using the standard authorization-code form fields and confidential client credentials. The response contains `app_id`, `app_family_id`, `manifest_digest`, `runtime_token`, `default_bucket_id`, and `buckets: [{id, name}]`. It does not return management access or refresh tokens.
6. Store `runtime_token` on the server and send it as the Engine execution token with the returned exact `app_id`. Runtime tokens do not expire automatically; they are dedicated, revocable credentials. Approved clients remain registered so the controller can revoke them later.

The manifest is bounded to 64 KiB and has this shape:

```json
{
  "schema_version": 1,
  "name": "support",
  "version": "1.0.0",
  "services": {
    "slack": {
      "version": "v1",
      "operations": ["chat.postMessage"]
    }
  }
}
```

Select 1–64 services and 1–1000 total operations. Operation IDs must be explicit and unique across the manifest because the existing execution protocol identifies an operation by `endpoint_name`. Wildcards, unknown manifest fields, and duplicate operation names fail registration. Service activation and provider credentials remain existing workspace setup responsibilities.

## Execution and revocation

`ExecuteRequest.bucket_id` is optional field 11. Omission uses the approved default. An explicit canonical UUID must belong to the immutable app's approved runtime bucket list, use a dynamic token, and pass the owner's current `bucket.use` authority. Engine checks routing before replay and again before credential resolution, and includes the selection in idempotency identity. Provider credentials never leave Engine.

Runtime consent credentials are pinned to the approved immutable app version. A later version cannot reuse that token. Existing family-wide default/service bucket mapping changes also cause the old credential to fail closed rather than silently adopt other credentials. Code-only container updates can keep their existing API app identity and credential; expanded integration requirements require a new app version and approval.

`POST /oauth/runtime/revoke` accepts form fields `client_id`, `client_secret`, and `token`. Revocation is idempotent and disconnects the client and subject identified by that dedicated runtime token, using the existing Connected Apps lifecycle. Disconnecting the application in Connected Apps revokes all of that subject's runtime tokens and pending runtime codes for the client. Reconnecting cannot revive the old credentials. Runtime-token validation reads current authority on every call.

The HTTPS/OAuth origin and native TLS gRPC address are configured separately. No same-origin assumption is made between those transports.

Controller disconnection through `/oauth/runtime/revoke` uses the presented runtime token to identify its client and consenting subject, then runs the existing Connected Apps disconnection lifecycle. It retires all runtime credentials and pending codes for that connection and removes its consent, releasing its dynamic-client quota slot. Unknown or already retired tokens return success.

## App-scoped deployment management

Minion requests `management: true` in the runtime registration JSON and uses OAuth scope `app.api.deploy`. The consent page explicitly authorizes publication and execution-token issuance for the single resulting API app family. Existing `api.execute` registrations remain execution-only and cannot upgrade through refresh.

A deployment authorization-code response includes the usual runtime fields plus standard OAuth `access_token`, `refresh_token`, `token_type`, `expires_in`, and `scope` fields. Keep the management access/refresh pair encrypted on the deployment controller. Only the independent `runtime_token` goes into the Minion workload. The deployment token cannot authenticate ordinary Engine management APIs or execute provider operations.

To publish another immutable release, send `POST /oauth/runtime/deploy` with `Authorization: Bearer <deployment access token>` and JSON `{ "manifest": "<exact compiler JSON bytes>" }`. The Engine rechecks the consenting subject's current authority and restricts app-management permissions to the original family. App creation and access to other families are excluded. The new declaration must retain the app name and approved provider versions, select only originally approved operations, and use the same approved bucket UUIDs. Immutable provider/operation identities and reviewed contract hashes are also checked before apply; a provider reimport using the same labels does not inherit consent. Broader selections or changed credential routes require new consent.

The response supplies the new version's dedicated execution token and public runtime metadata. Previous version-pinned tokens remain usable for rollback until explicitly revoked; the controller stores each new token under a fresh secret name. An unchanged contract can keep its existing app identity and credential.

Refresh management access at `/oauth/runtime/token` with `grant_type=refresh_token` and the confidential client credentials. Rotation preserves the original family and selection ceiling and does not rotate runtime credentials. Disconnect at `/oauth/runtime/revoke`, passing the controller's refresh token (or a dedicated runtime token) with client authentication. This uses the Connected Apps lifecycle to revoke the management grant, every release's runtime credentials, and pending codes. Refresh tokens are accepted only for rotation or revocation, never as deploy bearer credentials.

Contract discovery during a local Minion build currently uses a separate read credential. The deployment scope does not imply general catalog access or provider-credential administration.
