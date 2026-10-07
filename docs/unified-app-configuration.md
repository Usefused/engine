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
scheme name identify the credential. When auth is omitted, Fused checks credential
metadata in the service's selected bucket (including a per-service override).
If exactly one compatible scheme has all required credentials, planning selects
it. Multiple ready schemes require an explicit choice; no ready scheme retains
the provider's declared order and produces the missing-credentials warning.
Candidates must support every selected secured operation and satisfy all credentials
in their selected AND requirements. Anonymous and webhook-only services do not
trigger credential selection. Secret values are never read for this check.
The plan summary shows the resolved auth, which is saved in the immutable app
config. Apply uses that reviewed choice; adding another credential does not
silently switch an existing app. Explicit auth and managed references are preserved.
Existing immutable versions retain their reviewed policy; publish a new version
with auth omitted to use automatic credential selection.
An explicit choice must satisfy every selected secured operation, or planning
fails. Changing the app's choice does not modify its bucket credentials.

The same `auth` block works in `kind: sdk` and `kind: mcp` service entries.
For an existing app, deploy a new immutable version after changing authentication.

## Missing credentials during creation

New Unified App, SDK, MCP, and REST creation forms include **Generate execution
token**, enabled by default. Uncheck it to publish without issuing the initial
execution token; one can be created later. This sends `skip_token: true` on apply,
separate from the immutable YAML and compiled source. Existing app versions keep
their current family tokens. CLI initialization offers the same choice through
`fused-cli init --no-token`.

Unified App, SDK and MCP builders use Fused's `credential_readiness` plan result
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
compiling through the same Fused plan/apply API as the CLI. Use the service
picker to change providers or versions; the YAML editor preserves their exact
reviewed identities. Invalid YAML blocks compilation until corrected. Existing
apps keep their family name and default bucket when creating a successor.

The CLI can load a local `source_path`; advanced compiled deployments may instead
use `bundle_digest`. These source modes are mutually exclusive. Optional hosted
MCP metadata uses `mcp.description`; Fused enforces its permission and explicit
event-selection requirements. `language`, when supplied, must be `typescript`,
and Unified Apps cannot request SDK package generation.

## Editing with Fused AI

In the app editor, open **Edit with Fused AI** and describe a change or paste an
error in the prompt box. Fused sends the current TypeScript and exact selected operation
versions to the same Registry drafting API used by describe. Provider request and
response contracts ground the proposal; service selections, auth and buckets stay
unchanged. Review the current and suggested code, then choose **Apply to editor**
or **Discard**. Applying invalidates any previous compile plan. Validate and
compile, then deploy a new version separately.

If the code already matches the selected contract, Fused can return **No code
changes** with an explanation and next check instead of a code change. Pasted
errors are investigation context; the AI does not inspect your running Fused or
execute provider requests. A review without changes leaves your source intact.

Validation and compilation do not execute provider operations or verify that
customer/price IDs exist. Provider array/object schemas remain application data;
Fused handles wire serialization, including indexed nested form fields such as
`line_items[0][price]`. A Fused transport defect should be fixed in Fused, not
worked around by changing the app to send an incorrect provider data shape.

## Execution errors

Failed service calls preserve their error explanation in the Unified App result
and receipt. For JSON provider failures, Fused extracts the error message
(including Stripe's `error.message`) with the HTTP status. Input-validation
failures also identify the invalid or missing input before dispatch. Authored
code can catch these failures using `error.message` or let the app fail with the
same explanation. Replay preserves the message seen during the original run.

Full request/response bodies and stack traces still require
`app.unified_app.diagnostics.read`. Unknown provider response formats retain the
HTTP failure explanation; their raw body is available in diagnostics. Existing
receipts retain the message captured when they ran.

## Call another Unified App

Select an already deployed app by name and exact version:

```yaml
unified_apps:
  child:
    name: Customer lookup
    version: "1.0.0"
```

Call its alias from your TypeScript:

```ts
import { fused } from "@fused/unified-app";

const result = await fused.callApp("child", { customerId: "cus_123" });
// A failed child must not be treated as a successful lookup.
if (result.status !== "succeeded") throw new Error("Customer lookup failed");
const customer = result.output;
```

An app with `unified_apps` can omit `services`. Actor and owner need
`app.unified_app.use` on each dependency; restricted execution tokens must allow
`unified_app:child`. Each child keeps its own credentials, schemas, data and
execution record. Pass user references explicitly in the child input.

References stay pinned and must still receive traffic after promotion. Calls
are synchronous and do not retry automatically. Replay returns the recorded
child response without running the child again. A child can also declare its
own dependencies; calls back into an ancestor family are rejected.

A root execution permits 32 nested calls and eight app levels including itself.
Parent cancellation and deadlines bound the children. Each waiting parent keeps
its worker slot, so set `engine.unified_apps.max_concurrency` high enough for the
chain. A child fails immediately when capacity is full instead of waiting on an
ancestor's slot.
