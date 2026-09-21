# Publishing an Engine-owned OAuth registration

The broker uses ordinary bucket credentials and an exact locally stored service
auth contract. It no longer stores a second client pair or accepts a separate
`token_policy` in its publication API.

1. Enable the reviewed provider service/version on the broker Engine through
   the existing workspace flow. Confirm its OAuth authorization-code contract,
   HTTPS token endpoint, client authentication method, media type and PKCE policy.
2. Store the provider client pair in an operator-owned bucket through the normal
   credential setup flow, under the exact service and OAuth scheme.
3. Publish that registration through the operator-authenticated endpoint:

`PUT /managed-auth/broker/admin/apps/{serviceID}/{authName}`

```json
{
  "bucket_id": "11111111-1111-4111-8111-111111111111",
  "service_version_id": "22222222-2222-4222-8222-222222222222",
  "flow_name": "authorizationCode"
}
```

Use the configured broker administrator credential through the existing secure
operator workflow. The publication body contains references only; client pairs,
raw auth definitions and token policies are rejected. Unpublished bucket values
remain private. Publication admits an exact OAuth2 authorization-code scheme,
not API-key or OIDC configurations.

The `fused_oauth_publications` table stores the source identities and a fingerprint
of the approved client ID/auth contract. Exchange and refresh resolve credentials
through the same bucket reader as customer-owned OAuth. Token routing, encoding,
extra parameters and PKCE behavior come from the broker's exact service snapshot.
Caller-supplied auth/flow fields have no authority. Token requests use HTTPS and
never follow redirects, including same-origin redirects.

Publishing the same unchanged registration is idempotent. Client-secret rotation
in its bucket takes effect normally. A client-ID or auth-contract change fails
closed rather than retargeting existing grants. Publication remains pinned after
source deletion, preventing silent reassignment to a different bucket or version.
Publish a new named application and reconnect consumers for a different client
identity or contract. Never delete publication rows to bypass this fence.

## Cutover from the preliminary broker catalogue

Existing `fused_managed_auth_provider_apps` rows are no longer read, written or
created by this runtime. No automatic secret-copy migration is performed. Before
switching traffic, operators must configure the canonical bucket pair and reviewed
service snapshot, then explicitly publish the references above. The old registration
payload is rejected. Update all broker replicas together; old binaries still use
the previous model. Retire the unused legacy table through the operator's approved
data-retention/migration procedure after cutover and recovery verification.

## Installation enrollment cutover

Registry ticket issuance now requires the Engine's existing
`X-Fused-Installation-ID` header and validates ownership using the shared Engine
identity repository. Ticket redemption returns both `account_id` and
`installation_id`. Broker credentials are unique per account/installation pair;
renewing one Engine does not revoke another Engine under the same account.

Upgrade Registry first, then all broker replicas, then consumer Engines. Do not
run old account-scoped broker binaries alongside the new schema. The broker
migration revokes legacy grants without an installation ID; it does not guess an
identity. Old tickets without that identity cannot be redeemed. Consumers recover
through fresh Registry enrollment when their next broker refresh is rejected, so
schedule this coordinated cutover during a maintenance window: an old locally
unexpired access token can fail until the consumer next reconciles near expiry.

The consumer stores enabled/disabled intent independently from its credential.
`fused-cli workspace managed-auth disable` (or the Settings control) saves opt-out
before attempting remote revocation. `GET /workspace/managed-auth` reports
`status: disabled` and `revocation_pending` when broker acknowledgement is still
being retried. `DELETE /workspace/managed-auth` requires `workspace.update`, like
enable. Broker revocation is idempotent and does not require a valid Registry
license. Provider connections remain in the consumer database; already-issued
provider access tokens are not revoked by disabling managed auth.

Consumer startup and repeated enable requests reuse a healthy persisted grant.
One PostgreSQL transaction lock serializes enrollment/refresh across replicas,
including first issuance; attempts are bounded to 45 seconds. Broker rotation
replaces both hashes atomically. Failed local persistence or a lost response is
repaired by acquiring fresh Registry proof after the old refresh is rejected.
Transient errors preserve local credentials; they never delete user connections.
The background worker retries initial enrollment, renewal and pending revocation
every 30 seconds. It never overwrites saved disable. Explicit re-enable clears
pending local broker material and enrolls again for the same installation.

Each renewal now includes a fresh Registry ticket for the same account and
installation. Broker access lasts no longer than the original Registry ticket's
five-minute approval window. Thus suspension, license deletion or installation
withdrawal blocks new broker operations within five minutes of the last approval,
including tickets that were minted before withdrawal. In-flight operations already
admitted may finish. Registry also checks eligibility when a ticket is redeemed.
A Registry outage prevents renewal once the short approval expires.

Update Registry, broker and consumer binaries in a coordinated maintenance window:
old consumers omit the required renewal ticket, and old tickets lack issuing-license
identity or the approval deadline. Neither is silently upgraded.

Publication audience checks and verified cross-Engine webhook delivery use the existing broker installation identity. Consumer Engines continue to own encrypted provider tokens and their connection/refresh lifecycle. Central callback hosting and provider-specific webhook subscription lifecycles are separate capabilities; this publication model does not implement them.


## Multiple applications for one service

Keep the existing provider service manifest and its declared `auth_name`. The same operator-only PUT accepts a named application:

```json
{
  "managed_application_id": "729ea172-5512-4f37-b202-31084e2d2766",
  "name": "Team A support application",
  "owner_account_id": "33333333-3333-4333-8333-333333333333",
  "bucket_id": "11111111-1111-4111-8111-111111111111",
  "service_version_id": "22222222-2222-4222-8222-222222222222",
  "flow_name": "authorizationCode",
  "allow_all_enrolled": false,
  "allowed_consumers": [
    {
      "account_id": "44444444-4444-4444-8444-444444444444",
      "engine_installation_id": "55555555-5555-4555-8555-555555555555"
    }
  ]
}
```

Use canonical nonzero UUIDs. The owner is accountable Registry account metadata, not an automatic grant. Each allowed consumer matches both Registry account and Engine installation; granting one installation does not authorize sibling Engines under the same account. Named publications default to no consumers. `allow_all_enrolled: true` explicitly makes an offering available to every currently authorized enrolled installation. An unchanged PUT may update name and replace the complete audience. Supply all desired grants on every update. The owner, bucket, client identity and service contract remain pinned.

Omitting `managed_application_id` targets the permanently reserved default for `(service, auth_name)`, including existing publications and saved connections. Its original enrollment-wide access is preserved. Defaults cannot be reassigned or fall back to a named app. Named publication IDs are selected within the referenced service and scheme.

Discovery, code exchange and refresh recheck live installation and audience membership before resolving credentials. Withdraw access with operator-authenticated `DELETE /managed-auth/broker/admin/apps/{serviceID}/{authName}/applications/{applicationID}`. For the reserved default, omit `/applications/{applicationID}`. This clears every consumer grant without deleting or retargeting the publication, and still works if its bucket or credentials are unavailable. An ordinary publication update with `allow_all_enrolled: false` and `allowed_consumers: []` also withdraws access when its canonical registration is available. Existing webhook pulls and acknowledgements are denied as well. Already issued provider tokens remain consumer-owned and are not revoked at the provider; an operation already admitted may finish.

Consumers keep their managed `auth.ref` and add `auth.managed_application_id` to SDK/MCP config, or `--managed-application-id` to standalone CLI connect. Browser input sessions, OAuth sessions, user connections and refresh claims preserve the exact selector. The broker transport uses `/managed-auth/broker/connect/{serviceID}/{authName}/applications/{applicationID}/{client-id|exchange|refresh}`; omission uses the original route. No new managed-service manifest or dynamic auth scheme is required.

For verified webhook export, add the same `managed_application_id` to the existing `relay.publish` object beside `auth_name`. OAuth exchange can establish proofs only for that publication's exact webhook registrations. Refresh cannot rotate another application's proof, even when resource or token strings coincide. Consumer receivers continue referencing an existing local connection and broker webhook registration. The receiver supplies its connection’s saved application ID to the broker, which verifies it against the registration before creating a subscription. Receivers cannot choose provider app/resource claims.

A provider that returns standard OAuth tokens without webhook app/resource claims can use managed OAuth normally. The current relay requires signed-body verification and explicit provider claims; Google Drive channel creation/renewal and other provider-specific subscription lifecycles are not introduced here.

Upgrade Registry (which preserves the new selection fields), all broker replicas, consumer Engines, and CLI before selecting named applications. Do not mix old and new broker writers after the publication primary-key migration. The additive migration retains defaults, credentials and existing webhook policy hashes. Take the usual database backup before deployment. These changes do not restart local test Engines or migrate their databases until a new binary is started.
