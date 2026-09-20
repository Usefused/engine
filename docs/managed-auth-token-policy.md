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
A future versioned publication migration must explicitly handle consent/reconnect;
do not delete publication rows to bypass this fence.

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

This covers registration storage, the shared OAuth adapter, installation binding
and broker-credential recovery. Authenticated
broker audience binding, central consent
handoff, versioned publication migration and cross-Engine webhook delivery remain
unfinished. The customer Engine continues to own encrypted provider access/refresh
tokens and its existing connection/refresh lifecycle.
