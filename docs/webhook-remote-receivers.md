# Remote Engine webhook receivers

An app-level provider webhook can terminate at the broker Engine while a
consumer Engine owns the connected user's OAuth tokens. The broker verifies the
provider signature through ordinary webhook ingress. The consumer pulls only
its authorized events over HTTPS and publishes them into its own existing
`WEBHOOKS` stream. SDK webhook attachments and acknowledgements remain unchanged.
Self-hosted consumers need outbound access to the advertised broker; they do not
need a public callback URL for event delivery.

## Broker registration

Extend the existing signed `kind: webhook` registration with reviewed ownership
paths. For a Slack workspace bot installation:

```yaml
apiVersion: fused/v1
kind: webhook
name: managed-slack
callback_base_url: https://broker.example.com
services:
  slack:
    secret: ${bucket.managed-apps.secret.slack-signing}
    relay:
      publish:
        auth_name: oauth2
        token_resource_path: team.id
        token_app_path: app_id
        event_resource_path: team_id
        event_app_path: api_app_id
        event_id_path: event_id
```

The service must have a pinned, published managed OAuth application and a
signature policy v2 authenticating the complete raw body and a fresh timestamp.
A plaintext digest, unsigned input, or a signature covering only headers cannot
export resource claims. The operator must review that the event and token
response identify the same resource and app. These paths are provider contract
facts, never consumer-supplied ownership assertions. The initial implementation
accepts literal object paths and string identities; ambiguous arrays and provider
installations without sufficient proof are unsupported.

During a subsequent successful code exchange, the broker extracts those claims
from its own HTTPS provider response and binds them to the authenticated Engine
installation. It retains opaque grant identity, token hashes, and resource/app
identities. Provider tokens still return to and remain encrypted in the consumer
Engine; raw provider responses and client secrets are never returned as proof.
Existing connections established before export configuration require fresh
consent. [Slack's OAuth response](https://docs.slack.dev/reference/methods/oauth.v2.access/)
provides the `team.id` and `app_id` used in this example.

## Consumer registration

Apply an ordinary webhook configuration selecting an existing bucket-owned
managed connection and the broker registration ID:

```yaml
apiVersion: fused/v1
kind: webhook
name: slack-events
services:
  slack:
    relay:
      source:
        bucket: customer-connections
        connection_id: 11111111-1111-4111-8111-111111111111
        registration_id: 22222222-2222-4222-8222-222222222222
```

Plan/apply requires the existing service/config permissions and `bucket.use` on
the connection's actual bucket. Service identity and managed-source ownership
must match. This registration has no provider signing secret and rejects public
HTTP ingress even if its opaque slug is known. Attach the SDK to `slack-events`
and select the desired webhook names as usual.

The worker presents the provider token to its configured broker over the same
installation-authenticated HTTPS channel. The broker matches its stored proof
hash; supplying a workspace ID or another installation's token cannot authorize
subscription. Both app and resource must match each original verified event.
Every pull and acknowledgement rechecks installation validity, subscription
revocation, and the pinned registration/publication policy. HTTPS redirects are
rejected. HTTP is accepted only for literal loopback addresses in local tests.

## Durability and lifecycle

Each explicit remote receiver has its own JetStream durable cursor. Pulls scan
bounded batches, hold at most one unacknowledged delivery, and cannot retrieve
messages preceding the provider-proof creation time. Installation subscriptions
are capped at 128 active receivers. The existing stream retains events for 30
days; delivery after that retention horizon is not guaranteed.

The consumer verifies the response audience and current local connection,
publishes into its existing stream, records a provider-event receipt, and only
then acknowledges the broker using an audience-bound authenticated receipt.
Completed imports are deduplicated durably across process restarts, token
rotation, and provider retries. A stable JetStream message ID covers the narrow
publish-before-receipt crash window within the stream's configured deduplication
window. Delivery is **at least once**: a crash in that gap followed by an outage
longer than the JetStream deduplication window can repeat an event. SDK handlers
must remain idempotent; this does not claim exactly-once processing.

Config removal or connection deletion leaves durable receiver cleanup state
until remote withdrawal is acknowledged. Cleanup also covers setup requests
whose successful response was lost. Policy changes invalidate prior grants;
new proof requires fresh consent. Managed-auth disable stops outbound polling
and uses the existing installation revocation flow. Re-enrollment under a new
broker installation identity requires new provider proof for webhook delivery.

Delegated messages preserve the ordinary payload envelope and set explicit
`broker-verified` provenance in Engine-owned stream metadata. Original provider
headers, query parameters and the broker's ingress slug are withheld. They are
not represented as a provider signature verified by the consumer itself.

## Verification

The relay integration suite uses two isolated PostgreSQL schemas, two independent
file-backed JetStream servers, real broker HTTP handlers, and a TLS provider
fixture. It tests successful transfer, forged proof, cross-installation reads and
ACKs, wrong-resource filtering, retry deduplication, worker restart, connection
deletion, installation revocation, and routing-policy changes. Separate ingress
coverage verifies signatures and rejects HTTP requests to remote-only rows.

Run with an explicitly selected test PostgreSQL server:

```sh
DATABASE_URL='postgres://user@127.0.0.1:5432/test?sslmode=disable' \
  go test -race -tags headless ./internal/engine/webhookrelay
```

The tests create and remove only their own UUID-named schemas. They never reset
a live Engine database or its credentials.
