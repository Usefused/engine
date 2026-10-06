# Webhooks in the Fused UI

Open **Webhooks** in the sidebar to find registered receiving URLs. Search by name or URL, filter by service, and use the compact copy icon beside a URL. Each service's Webhooks tab also links to its receiving URLs and the creation form.

URL discovery requires `service.read` for the specific service. Fused checks this permission for direct GraphQL requests too; hiding a button is not the authorization boundary. Discovery returns signing-secret presence, never its value or reference. Managed receivers are identified separately and do not advertise a provider callback URL.

## Create a receiving URL

Choose **Create webhook**, enter a unique registration name, select a service, and provide the public Fused URL. Add a signing-secret reference from Credentials when required by the provider. Review, then create the registration and copy its URL into the provider's webhook settings.

The form uses the same `/webhook-config/plan` and `/webhook-config/apply` APIs as the CLI. Creation requires `app.webhook.create`, the selected service's permissions, and applicable bucket and ownership permissions. The creation form rejects existing names to avoid reconciling a multi-service registration down to one service.

## Manage a signing secret

The compact **Manage secret** link opens the exact bucket and named variable in Credentials. Fused returns that link only when the caller has both `bucket.read` and `credentials.metadata.read` for the referenced bucket. The value remains masked; replacing it requires the existing `credentials.manage` permission. The editor preserves the current expiry and does not create a missing variable implicitly.

**Change secret reference** lets webhook managers select a different bucket or variable name. Store the new variable in Credentials first, then review and save its reference. This changes only this webhook binding, avoiding a rename that could break other consumers of the original variable.

`POST /webhook-config/signing-secret/plan` accepts `slug` and `secret`. Fused requires workspace `app.webhook.manage`, service read access, existing ownership authorization, service consumption, and bucket-use permissions. It patches the complete stored desired configuration and returns a normal plan receipt. `/webhook-config/apply` rechecks authorization and generation before applying it. The receiving URL, other services, callback configuration, and relay settings are retained. Concurrent changes require a fresh review. Empty or raw-secret replacements are rejected.

For signature policies that pin an exact secret reference, rotate the credential value in Credentials using that reference; switching to a different reference remains subject to Fused's existing signature-policy validation.
