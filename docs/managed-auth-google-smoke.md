# Google managed-auth acceptance check

Verified locally on 2026-09-20 with the registered callback
`http://localhost:8081/workspace/connect/callback` and the existing Google test app.
No application secret or provider token is included in this report.

The opt-in test is `TestGoogleManagedAuthLive` in
`internal/engine/api/managed_auth_google_live_test.go`. It uses two independent
PostgreSQL databases, one for the broker and one for the consumer. Registry
licensing and workspace admission use test fixtures; provider consent, callback
handling, publication resolution, broker HTTP operations, encrypted connection
persistence and the refresh coordinator use the real implementations.

The live check passed:

1. Google accepted the configured application and loopback callback with PKCE.
2. The normal callback handler exchanged the authorization code through the broker.
3. The customer database stored an encrypted refreshable provider connection.
4. The broker database contained no user connections, and the customer database
   contained no OAuth application secrets.
5. The issued access token successfully called Google's OpenID userinfo endpoint.
6. The existing refresh coordinator renewed the connection through the broker
   and committed its encrypted result using the ordinary refresh lease.
7. Disable withdrew broker access while preserving the customer connection;
   explicit re-enable acquired fresh broker authority.

The separate PostgreSQL lifecycle suites cover saved opt-out across restart,
revocation retry during outages, sibling-installation isolation, approval deadlines,
account suspension, deleted licenses, failed local saves and lost responses.

## Repeating the live check

Create two disposable local Engine databases named `fused_google_broker_test` and
`fused_google_consumer_test`. Supply their PostgreSQL URLs through
`FUSED_GOOGLE_BROKER_DATABASE_URL` and `FUSED_GOOGLE_CONSUMER_DATABASE_URL`.
Provide `FUSED_GOOGLE_OAUTH_ID` and `FUSED_GOOGLE_OAUTH_SECRET` through a private
local environment file or secret manager; do not place them on the command line,
in committed configuration or in test output.

From the Engine module, run:

```sh
FUSED_GOOGLE_LIVE_TEST=1 go test -tags headless ./internal/engine/api \
  -run '^TestGoogleManagedAuthLive$' -count=1 -v -timeout 10m
```

The test writes the authorization URL to a private temporary navigation file at
`/private/tmp/fused-google-managed-auth-url`. Open it in a browser and complete
Google consent within eight minutes. The scopes are limited to `openid`, `email`
and `profile`. The URL file is removed by test cleanup. Remove the disposable
databases and the temporary credential input after collecting the result.

This check does not prove a deployed Registry/broker installation, authenticated
broker audience binding, central callback handoff or webhook delivery. Those
remain separate production acceptance work. The test preserves the Google app's
existing account authorization; it does not globally revoke other grants for the
same application.

## Separate Engine process acceptance

A second live Google run passed on 2026-09-20 using the normal headless
`cmd/engine` executable in two independent OS processes:

| Component | HTTP port | PostgreSQL database |
| --- | --- | --- |
| Broker Engine | 18082 | `fused_process_broker_test` |
| Consumer Engine | 8081 | `fused_process_consumer_test` |
| Local Registry harness | 18080 | `fused_process_registry_test` |

The Engines also used separate NATS listeners and JetStream directories. The
Registry harness used production handshake, heartbeat and enrollment handlers,
including PostgreSQL ticket issuance/redemption and installation ownership checks.
It seeded two disposable licensed accounts. Google service metadata and initial
workspace service enablement were fixtures; external identity provisioning and
the Registry import/GraphQL implementation were outside this check. Engine
control-plane authentication, bucket admission and connect handling were real.

Observed results:

- Registry persisted two distinct installations and four redeemed enrollment
  tickets during initial enrollment, authority renewal and explicit re-enable.
- The broker publication API accepted the pinned ordinary bucket/contract
  registration. Consumer managed-auth status became `ready`.
- Fresh browser consent completed through the consumer callback. The consumer's
  encrypted access token returned HTTP 200 from Google's userinfo endpoint.
- After the test marked the consumer connection expired and restarted the
  consumer binary, the normal startup worker reported `claimed=1 attempted=1
  refreshed=1`, with zero failures. The refreshed token also returned HTTP 200.
- The broker stored zero provider connections; the consumer stored one encrypted
  managed connection and zero application-secret records.
- DELETE `/workspace/managed-auth` returned `disabled` with no pending revocation.
  The consumer's broker installation was revoked while the other installation
  stayed active. The consumer's local broker credential was removed, and its
  Google connection was retained.
- After another consumer process restart, status remained `disabled`, and a new
  managed connect request was rejected before session creation.
- Explicit PUT re-enable returned `ready` with fresh broker authority. The
  existing Google connection remained usable.

Fixture support is in `testutil/managedauthprocess/main.go` and the root repository's
`backend/cmd/registry-demo/main.go`. The helper prepares ordinary encrypted bucket
records and contract snapshots; it does not run an alternative Engine runtime.
Its `FIXTURE_MODE=nats` starts the isolated buses, `broker`/`consumer` prepare the
named disposable databases after Engine startup, and `verify` checks the saved
consumer token against Google without printing identity data. Setup takes
`DATABASE_URL`, `METADATA_FILE`, `BUCKET_OUTPUT_FILE`, and the Engine's base64
`FUSED_ENCRYPTION_KEY`; only broker setup receives the Google client pair. The
Registry harness takes `LICENSE_OUTPUT_FILE` for mode-0600 license output and
`SERVICE_FIXTURE_FILE` for credential-free GraphQL provider metadata.

This proves local communication between independently running Engines. It does
not establish production deployment readiness: authenticated broker audience
binding, deployed infrastructure, central callback handoff and webhook delivery
remain separate work. No production Engine or saved CLI configuration was changed.
