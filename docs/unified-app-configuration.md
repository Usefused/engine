# Unified App configuration

Unified Apps, SDKs and MCP servers use the same per-service authentication config.
In the UI, select a scheme under **Service authentication**. With the CLI, set
`services.<service>.auth.type` and `auth.name` in the app YAML:

```yaml
apiVersion: fused/v1
kind: unified_app
name: subscription-checkout
version: 1.0.1
bucket: default
source_path: app.ts
services:
  "@your-provider/stripe":
    version: "2026-07-29.dahlia"
    operations: [postCustomers, postCheckoutSessions]
    auth:
      type: bearer
      name: bearerAuth
```

Use the service reference and version from your workspace. Both the type and
scheme name identify the credential; storing `bearerAuth` does not make it the
default if the provider declares another security alternative first. Without an
explicit preference, Engine follows the provider contract and applicable connect
requirements. It does not search stored secrets to choose an alternative.
An explicit choice must satisfy every selected secured operation, or planning
fails. Changing the app's choice does not modify its bucket credentials.

The same `auth` block works in `kind: sdk` and `kind: mcp` service entries.
For an existing app, deploy a new immutable version after changing authentication.

## Missing credentials during creation

Unified App, SDK and MCP builders use Engine's `credential_readiness` plan result
to warn before publishing when selected credentials are missing. The warning
names each service, auth scheme and its resolved bucket, including per-service
overrides. **Add credentials** opens the bucket in a new tab so the app draft
remains intact. **Recheck and continue** creates a fresh plan and continues only
when its credential requirements are met. **Proceed anyway** explicitly accepts
deferred setup; the created-app screen retains the warning. **Cancel** creates
no app.

Bucket links require bucket read and credential-management access. Other app
creators see the missing requirements and can ask a bucket administrator for
help or proceed without changing access. This checks stored required material,
not provider validity or every future connected user's OAuth token. Ready,
anonymous and broker-managed selections do not receive a missing-secret warning.

## TypeScript and YAML in the UI

The **TypeScript / YAML config** switch edits two parts of one draft:

| File | Contents |
| --- | --- |
| `app.ts` | `buildUnifiedApp`, input/output schemas and integration logic |
| `app.yaml` | Name, semantic version, description, default bucket and service configuration |

Service configuration includes exact versions, operation allowlists, `auth`,
`connect.scopes`, bucket overrides, request `injections`, and webhook event
selections. `webhook_attachment` identifies an applied webhook registration.
OAuth/OIDC can use the existing `auth.ref` syntax to reuse an eligible credential.
Credential values stay in buckets.

The UI keeps `source_path: app.ts` and sends the current TypeScript inline when
compiling through the same Engine plan/apply API as the CLI. Use the service
picker to change providers or versions; the YAML editor preserves their exact
reviewed identities. Invalid YAML blocks compilation until corrected. Existing
apps keep their family name and default bucket when creating a successor.

The CLI can load a local `source_path`; advanced compiled deployments may instead
use `bundle_digest`. These source modes are mutually exclusive. Optional hosted
MCP metadata uses `mcp.description`; Engine enforces its permission and explicit
event-selection requirements. `language`, when supplied, must be `typescript`,
and Unified Apps cannot request SDK package generation.
