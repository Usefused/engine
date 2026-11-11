# Webhooks in the Fused UI

Open **Webhooks** in the sidebar to browse registrations by name, service, and setup status. Search by name or URL, or follow the link from a service's Webhooks tab to filter by that service. Open any row to copy the receiving URL, manage verification, or view Analytics and Events.

URL discovery requires `service.read` for the specific service. Fused checks this permission for direct GraphQL requests too; hiding a button is not the authorization boundary. Discovery returns signing-secret presence, never its value or reference. Managed receivers are identified separately and do not advertise a provider callback URL.

## Create a receiving URL

Choose **Create webhook**, enter a unique registration name, select a service, and provide the public Fused URL. Add a signing-secret reference from Credentials when required by the provider. After you review and create it, the new webhook's Overview opens so you can copy its URL into the provider's settings.

The form uses the same `/webhook-config/plan` and `/webhook-config/apply` APIs as the CLI. Creation requires `app.webhook.create`, the selected service's permissions, and applicable bucket and ownership permissions. The creation form rejects existing names to avoid reconciling a multi-service registration down to one service.

## Manage a signing secret

The compact **Manage secret** link opens the exact bucket and named variable in Credentials. Fused returns that link only when the caller has both `bucket.read` and `credentials.metadata.read` for the referenced bucket. The value remains masked; replacing it requires the existing `credentials.manage` permission. The editor preserves the current expiry and does not create a missing variable implicitly.

**Change secret reference** lets webhook managers select a different bucket or variable name. Store the new variable in Credentials first, then review and save its reference. This changes only this webhook binding, avoiding a rename that could break other consumers of the original variable.

`POST /webhook-config/signing-secret/plan` accepts `slug` and `secret`. Fused requires workspace `app.webhook.manage`, service read access, existing ownership authorization, service consumption, and bucket-use permissions. It patches the complete stored desired configuration and returns a normal plan receipt. `/webhook-config/apply` rechecks authorization and generation before applying it. The receiving URL, other services, callback configuration, and relay settings are retained. Concurrent changes require a fresh review. Empty or raw-secret replacements are rejected.

For signature policies that pin an exact secret reference, rotate the credential value in Credentials using that reference; switching to a different reference remains subject to Fused's existing signature-policy validation.

## Registration analytics and received events

Each detail route identifies both the service and the receiving slug. Overview shows the provider-ready URL and verification in separate cards. Analytics and Events require `service.read` for that service and workspace `audit.read`; ordinary readers keep access to setup without seeing event history. Events show the recorded service on each receipt and stack the row when space is limited. Select a receipt to see its message ID, payload size, delivery time, and retries.

Analytics shows received, delivered, failed, and verification-rejected counts plus average and P95 latency. UTC date bounds apply to summaries and event history. Outcome drill-downs open Events with a server-side failure or rejection filter; event type, dates, and pagination also filter in SQL before totals and pages. Failures include rejected verification attempts and failed delivery receipts. Opening a receipt exposes the recorded failure reason, verification, timing, retries, and correlation metadata.

Reporting uses only `fused_engine_execution_events`, scoped by the authenticated account, service, and exact registration ID (`webhook_id`). No parallel analytics store or provider payload collection is introduced. Older receipts without a registration ID remain available in service-wide Activity and cannot be reliably attributed to an individual receiving URL.

The Engine advertises an explicitly registered callback URL unchanged. Registrations without one use the same loaded `engine.public_url` configuration as the dashboard, including `FUSED_ENGINE_PUBLIC_URL` overrides. Request Host/forwarding headers and browser origins never invent a provider destination. Restart the Engine after changing deployment URL configuration; managed receivers continue to advertise no provider callback URL.

For isolated UI verification, build the UI and run `node scripts/webhook-details-browser-fixture.mjs` from `ui`. The fixture uses synthetic data only, supports success/empty/error/missing/managed/access-error modes and both/service-only/audit-only/neither roles, and never calls a live Engine or Registry.

## Receipt metadata and private messages

Receipt details show the recorded service, execution outcome, verification and delivery states, failure reason/category/code, attempts/retries, payload size, latency, start/completion/recorded timestamps, and message/receipt/registration/service/version/app/trace/span correlation identifiers. Missing historical metadata remains “Not recorded”. Receipt and analytics queries never contain message bodies, request headers, query parameters, credential material or body/idempotency hashes.

**Private webhook message** follows the Unified App private-diagnostics pattern with the separate service-scoped `service.webhook.diagnostics.read` permission. Revealing a body also requires `service.read` for that service and workspace `audit.read`. Ordinary read/manage and Admin grants do not imply diagnostic access; Owner retains its existing all-permissions contract. The management API checks the exact account, service, registration and retained canonical receipt on every reveal. Runtime app tokens cannot use this endpoint.

For new requests that produce an ingress receipt, Fused captures the original body before normalization, including unverified requests rejected by signature verification. It retains at most 64 KiB, labels truncation, and represents non-UTF-8 bytes as base64. Headers, cookies, authorization/signature values and query parameters are excluded. Evidence is encrypted with a separate per-record data key, is immutable, expires after seven days (or when shorter canonical receipt retention removes access), and is cleaned up in bounded retention batches. Capture failure never changes provider acknowledgement or retries provider work.

The message is fetched only on explicit expansion, with `Cache-Control: no-store`, no shared/client persistence, cancellation on collapse/navigation, and exclusion from Fused assistant context. Historical bodies cannot be backfilled; uncaptured, expired and inaccessible messages show an unavailable state. GET webhooks have an empty captured body; excluded query parameters are never reconstructed as a message.
