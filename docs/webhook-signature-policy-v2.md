# Authenticated webhook challenges and timestamped signatures

Signature policy version 2 extends the shared verifier with three provider-neutral
fields. It uses the existing `incoming_webhook_config.signature_policy` contract,
private bucket key resolver, HTTP ingress and JetStream publication path.

- `components[].kind: constant` contributes the exact `value` bytes, including
  intentional whitespace. Components remain ordered; `component_separator`
  appears between them.
- `verification.signature.timestamp` identifies one signed Unix-seconds header.
  `max_age_ms` must be positive and at most 300000; `max_future_ms` must be between
  zero and 300000. Validation requires this header among the signed header inputs.
  Runtime rejects missing, duplicate, malformed, stale or excessively future timestamps.
- A `kind: challenge` rule can combine `verification.kind: signature` with a
  sibling `response`. The response value must come from the body, and the recipe
  must sign the raw body. Runtime verifies the signature before returning the
  challenge and never publishes challenge requests as events.

For Slack, the reviewed recipe is HMAC-SHA256 over constant `v0`, the
`X-Slack-Request-Timestamp` header and the raw body, separated by `:`. The digest
is hex-encoded and must carry the `v0=` prefix in `X-Slack-Signature`. The signing
key remains a bucket reference. The complete credential-free example is in
[the shared fixture](../../contract-fixtures/signature/v2_authenticated_challenge.json).
Its bucket and key names are test placeholders, not production configuration.

Both Registry admission and Engine validation reject v2 fields in a v1 policy.
Registry derives `webhook.signature.recipes.v2` in addition to the base recipe
capability when a version has webhooks using a v2 policy. Engines advertise support
explicitly. The GraphQL schema, both Engine metadata queries, and CLI JSON/YAML
transport retain these fields. Existing v1 standalone challenges remain explicit
legacy behavior; they do not gain authentication merely by upgrading the Engine.
Signature credentials now require a matching declared prefix and a single value.

Timestamp validation bounds replay age; it is not event deduplication. The existing
publisher assigns message IDs independently of a provider's event identity.
Cross-Engine recipient authorization, provider event deduplication and remote
acknowledgements are separate work. This policy does not authorize forwarding to
another Engine or prove ownership of a provider workspace.

Coverage includes verifier tampering and freshness tests, signed versus unsigned
challenge HTTP ingress, no publication on rejection or challenge, GraphQL field
projection, metadata decoding, CLI round trips and capability negotiation. Live
Slack URL verification and broker-to-consumer event delivery are not established
by these local tests.
