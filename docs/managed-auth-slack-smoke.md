# Slack managed OAuth and signed-event acceptance

Status: live managed Slack OAuth and signed webhook delivery passed on 2026-09-20
between two independently running Engines. A real user-authored `app_mention`
passed broker verification, reached the consumer's existing WEBHOOKS stream, and
was durably acknowledged. A generated TypeScript SDK handler and a live MCP
resource listener both subsequently received the same fresh Slack event.
Keep the test databases, processes and private credential input available after
the run unless the user asks for cleanup.

## Current local setup

Two independent headless Engine processes are running on ports 18082 (broker) and
8081 (consumer), with separate PostgreSQL databases and separate NATS listeners.
The local Registry harness is on port 18080. Both Engines report managed auth
`ready`. The broker has two encrypted OAuth application rows and one encrypted
`slack_signing` bucket secret; the consumer has zero application-secret rows.
The user confirmed PKCE and token rotation are both disabled for the test app.
The successful callback reached the consumer at 01:09:36 local time. Its encrypted
managed bot token returned `ok: true` from Slack `auth.test` at 01:10:02. The broker
stored zero provider connections, and the consumer retained zero application-secret
rows. No refresh token was issued; this run does not claim token-refresh coverage.
Saving the exact redirect URL resolved an initial Slack configuration rejection.
A later sign-in error occurred in the embedded browser; a fresh authorization in
the regular browser completed successfully. This does not establish the cause of
the embedded-browser error.
The reviewed OAuth fixture uses client-secret POST authentication, a comma scope
delimiter, optional refresh tokens, and only `app_mentions:read`.

Private process configuration, PIDs, logs and the test driver are preserved under
`/private/tmp/fused-slack-process`; credential input remains at
`/private/tmp/fused-slack-live.env`. These files are not checked into the repository.
The official cloudflared 2026.9.1 release was downloaded and its release digest
verified. A separate local proxy on port 18085 limits the public tunnel to the
consumer callback, a non-sensitive health check, and the one applied
`slack-signature-test` webhook route. Public probes confirmed
`/workspace/managed-auth`, `/managed-auth/broker/admin/apps`, and `/graphql` all
return 404.

The exact HTTPS callback is stored in the private environment's `public-url.json`
plus `/workspace/connect/callback`; restarting the quick tunnel may change its
hostname. Do not change the user's saved CLI target or deploy this fixture to
production. Provider identity verification used Slack's read-only `auth.test`
endpoint and checked its `ok` field, without logging returned identity data.
The focused connectauth, webhookverify and signaturepolicy regression suites and
fixture-helper vet checks passed. These tests do not prove Slack webhook acceptance.

## Intended ownership

The Fused broker owns the Slack application's OAuth client pair and signing
secret in ordinary private bucket records. The consumer owns its encrypted Slack
installation tokens and uses the existing broker-assisted exchange/refresh path.
The signing secret is not exposed through a consumer-readable Fused reference.

Slack sends Events API requests to an app-level public HTTPS endpoint on the
Fused Engine. Its existing webhook ingress verifies the request before replying
to a challenge or admitting an event. Authorized delivery then reaches the
consumer's existing webhook stream and subscribers. Consumer authorization must
be established from verified provider installation/resource identity, not an
arbitrary claimed `team_id` or destination URL. Multiple installations visible to
one event require explicit authorized recipients; a workspace ID alone does not
establish the correct consumer.

## Provider evidence and contract mapping

| Official evidence | Required behavior | Intended Engine representation |
| --- | --- | --- |
| [Request verification](https://docs.slack.dev/authentication/verifying-requests-from-slack/) | HMAC-SHA256 over literal `v0`, request timestamp and untouched body, separated by colons; signature has `v0=` prefix; check timestamp freshness | Generic signature recipe with constant input and timestamp validation; app secret remains a bucket reference |
| [URL verification](https://docs.slack.dev/reference/events/url_verification/) | Verify authenticity before returning the challenge | Authenticated challenge response in the same verification policy |
| [HTTP Events API](https://docs.slack.dev/apis/events-api/using-http-request-urls/) | Public app-level request endpoint, prompt acknowledgment and retries | Existing ingress and durable webhook delivery; authenticated remote Engine recipient binding |
| [OAuth installation](https://docs.slack.dev/authentication/installing-with-oauth/) | HTTPS redirect, comma-separated scopes, `oauth.v2.access` exchange, distinct bot/user grants | Reviewed OAuth contract; start with a bot grant and one event scope |
| [Token rotation](https://docs.slack.dev/authentication/using-token-rotation/) | Rotation depends on app configuration | Verify the test app's setting before requiring refresh tokens; do not change production app settings |

## Signed ingress implementation and remaining work

The shared [signature policy v2](webhook-signature-policy-v2.md) implements constant
message components, signed timestamp freshness, strict signature prefixes and
single-value credentials, and signature verification before a challenge response.
Registry capability negotiation, GraphQL projections, both Engine metadata reads,
and CLI JSON/YAML transport preserve these semantics. Regression tests prove
rejected requests and authenticated challenges do not publish events, while valid
events use the existing publication path. The checked execution schema and
extension catalogue are updated.

The broker now runs the compiled v2 verifier. The Registry test harness was
restarted with its original validated license hashes, and serves the same
credential-free metadata through the batch webhook query. Consumer, NATS and
tunnel processes were preserved. Both Engines still report managed auth `ready`,
and the preserved consumer token passed Slack `auth.test` again after the restart. Registration used the normal authenticated
`/webhook-config/plan` and `/webhook-config/apply` routes with the existing private
`slack_signing` bucket reference. No production configuration was changed.

Real HTTP probes against this broker passed:

- Unsigned challenge, wrong digest, missing prefix, body tampering, stale/future
  timestamps, duplicate signature and duplicate timestamp: HTTP 401, no stream
  sequence advance.
- Signed challenge: HTTP 200 with the expected challenge, no stream sequence
  advance.
- Signed synthetic `app_mention` event: HTTP 200 and exactly one additional
  durable broker stream entry. Consumer stream sequence remained unchanged.
- Public HTTPS tunnel: signed challenge HTTP 200; unsigned challenge HTTP 401.
  Control endpoints still return HTTP 404 through the tunnel.

Private evidence is in `webhook-probe-evidence.json` and
`public-probe-evidence.json` under the preserved test directory. These records
contain outcomes and stream counters, not signing secrets or message bodies.
Slack's Event Subscriptions page showed the exact broker Request URL as Verified.
The broker stream remained at sequence 1 (the synthetic test event), and the
consumer remained at sequence 0 after verification, confirming no challenge was
published as an event.

Live provider-origin ingress passed on 2026-09-20. Socket Mode was initially on,
which routed events away from the configured HTTP Request URL. After Socket Mode
was switched off and the app joined the test channel, a fresh user-authored
`app_mention` reached the broker. The audit at 02:04:23 Europe/London reports
`verification_status=verified` and `delivery_status=ingested`, with no failure
reason. The durable broker stream advanced to sequence 2, containing one real
Slack mention in addition to the synthetic event. The consumer remained at
sequence 0, as expected until cross-Engine forwarding exists. Private
`slack-event-evidence.json` records these counters without message content.

[Remote Engine receivers](webhook-remote-receivers.md) now implement explicit
provider-response proof, installation-bound pull subscriptions, and transfer into
the consumer's existing WEBHOOKS stream. The two-Engine PostgreSQL/JetStream
integration tests pass, including isolation, restart, duplicate suppression,
disconnect, revocation and policy-change rejection. The initial transport is
outbound pull, so a self-hosted consumer needs no public webhook endpoint.

Both preserved local Engine processes have been upgraded. Broker and consumer
relay configurations were applied through the normal plan/apply routes. Fresh
narrow-scope consent recorded the provider's app/workspace proof. The user's new
mention then reached the consumer: broker sequence 4 and consumer sequence 1
contain the same provider event ID. Consumer provenance is `broker-verified`,
provider transport headers are withheld, and one durable consumer receipt exists.
The broker acknowledgement floor is 4 with zero pending acknowledgements. The
broker still stores zero provider connections. Private `relay-live-evidence.json`
records these assertions without tokens or message content. Separate automated
tests cover proof updates after refresh and the subscription limit; Slack token
rotation remains disabled, so live Slack refresh has not been tested.
After upgrading and gracefully restarting both Engines again, the same event and
receipt counts remained unchanged, the broker still had zero pending
acknowledgements, and the consumer token again passed Slack `auth.test`.
The live environment, tunnel, database records and credentials remain preserved.

## SDK and MCP runtime acceptance

On 2026-09-20, normal consumer Engine plan/apply published
`slack-managed-sdk-test@1.0.0` and `slack-managed-mcp-test@1.0.0`. Both select only
Slack `auth.test` and `app_mention`, use the existing `slack-process` bucket and
managed connection, and attach to `slack-managed-events`. The disposable local
catalogue was extended with reviewed operation/event metadata using the production
snapshot store; no application credentials or provider connections were reseeded.

- The generated TypeScript SDK called Slack `auth.test` over consumer gRPC and
  checked both transport success and Slack's `ok` value. Its ordinary generated
  webhook handler consumed the retained mention and a fresh user-authored mention,
  acknowledging both. The SDK durable has zero pending acknowledgements.
- MCP discovery and `search_docs` exposed the exact selected scope. An `execute`
  call to `auth.test` passed using the same consumer-owned connection.
- The Engine's current `2026-07-28` MCP surface exposed the selected event through
  `resources/list` and `resources/read`. A `subscriptions/listen` SSE listener
  acknowledged that exact URI, received `notifications/resources/updated`, and
  read the fresh occurrence. This support is newer than the installed skill text
  that describes MCP webhooks as unsupported.
- Event identity hashes match between the generated SDK handler and MCP resource
  read. The final fresh event is broker sequence 5 / consumer sequence 2; both
  relay and SDK acknowledgements are settled. SDK credentials cannot access the
  MCP app (HTTP 401), and unselected MCP event resources are denied (HTTP 400).

The SDK app was published with `generate: false`, then its matching package was
generated and compiled locally with the repository's deterministic TypeScript
generator. This verifies generated-client runtime behavior, not Registry package
generation, cache, or download. MCP used the actual running Engine protocol and
tool runtime. Slack `auth.test` is a read-only identity check with no additional
scope requirement ([provider documentation](https://docs.slack.dev/reference/methods/auth.test/)).

Private `sdk-mcp-live-evidence.json` records only outcomes and counts. The SDK/MCP
configs, generated package, one-time app tokens and test scripts remain under the
existing private test directory. No provider secrets were supplied to either client.

## Acceptance sequence

1. Use a dedicated Slack app and test workspace. Obtain its client ID, client
   secret and signing secret through private local input. Confirm token rotation
   settings and expose HTTPS routes for the consumer callback and broker ingress.
2. Prove signature verification locally: valid request, altered raw body, wrong
   signature, missing/duplicate timestamp or signature headers, stale/future
   timestamp, and authenticated versus unauthenticated challenge. Invalid requests
   must not publish events. Legitimate provider retries need deliberate delivery
   deduplication; timestamp freshness alone does not reject every replay.
3. Launch separate broker and consumer Engine processes with separate databases
   and normal Registry enrollment. Install the Slack app through managed OAuth;
   confirm client/signing secrets remain broker-only and provider tokens remain
   consumer-owned. Call a read-only identity endpoint with the resulting token.
4. Establish a verified installation-to-consumer event subscription. Reject a
   second consumer claiming someone else's Slack installation and reject arbitrary
   delivery URLs. Test expired/revoked receiver authorization.
5. Configure the Slack Events API Request URL and complete its signed challenge.
   Turn Socket Mode off so Slack uses the HTTP Request URL.
   Subscribe to one narrow event, such as `app_mention` with its documented scope.
   Have the user generate the event in the test workspace; the agent must not send
   Slack messages without explicit authorization.
6. Verify delivery only to the authorized consumer/subscriber, durable recovery
   after a receiver restart, and retry/deduplication behavior. Do not log message
   bodies, OAuth tokens, signatures or secrets as acceptance evidence.
7. If token rotation is enabled, force a test connection due and verify the normal
   consumer refresh worker renews it through the broker. Test disable/re-enable
   without deleting the provider connection.

OAuth-only success and signed broker ingress success are separate milestones;
neither proves consumer delivery until the final routing assertions pass.
