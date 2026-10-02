# Trigger a Unified App from a webhook

Unified Apps consume provider events from the existing Engine `WEBHOOKS` NATS stream. Register provider ingress with `kind: webhook` first, then attach that registration to the app and select event names alongside its operations:

In the Unified App editor, select exact webhook events in the service picker and enter an applied webhook registration that covers those services. The editor links to registration creation when one is needed. `fused-cli describe '<goal>' --kind unified` can also resolve event intent, reuse a compatible registration, or create one before deploying the app. Review the generated TypeScript and webhook scope before confirming the describe proposal.

```yaml
apiVersion: fused/v1
kind: unified_app
name: issue-handler
version: "1.0.0"
bucket: default
source_path: issue-handler.ts
webhook_attachment: team-events
services:
  jira:
    version: "<enabled-service-version>"
    operations: [getIssue]
    webhooks: [issue.created, issue.updated]
```

Use the exact operation and event names from your service contract. An app may select events without any provider operations when its code only validates or transforms the event. `webhooks_select_all: true` selects every event for that service within the attached registration. Omitted event selections do not start a consumer. The existing app plan/apply flow validates the attachment and its service coverage. Trigger changes require a new immutable app version.

```sh
fused-cli webhook apply -f .fused/webhooks/team-events.yaml
fused-cli unified-app plan -f .fused/unified_app/issue-handler.yaml
fused-cli unified-app apply -f .fused/unified_app/issue-handler.yaml
```

The hosted consumer starts automatically after apply, normally within five seconds. No SDK receiver process or additional public endpoint is needed. Direct registrations retain existing signature verification; managed webhook relays enter the same event stream.

## Input contract

The app's existing `execute({ input })` receives the same normalized JSON envelope delivered to SDK webhook receivers:

```json
{
  "body": { "issue": { "id": "123" } },
  "headers": {},
  "query": {},
  "path": { "urlSlug": "team-events-jira", "eventName": "issue.created" }
}
```

Declare the envelope fields your app uses in its input schema and map the provider body inside TypeScript. For example:

```ts
import * as z from "zod/mini";
import { buildUnifiedApp } from "@fused/unified-app";

export default buildUnifiedApp({
  input: z.object({ body: z.object({ issue: z.object({ id: z.string() }) }) }),
  output: z.object({ issueId: z.string() }),
  // Normalize the provider envelope before returning an app-specific result.
  execute({ input }) {
    return { issueId: input.body.issue.id };
  },
});
```

Existing selected operation bindings and `fused.forUserRef(...)` work inside this function. Map a trusted provider account identifier to the appropriate connected-user reference in app code; Engine does not infer a user or connection from arbitrary webhook fields. REST and MCP calls to the same app continue to use that same declared input schema.

## Delivery and lifecycle

- Each family has its own durable NATS consumer. Multiple Engine replicas share that consumer, while unrelated apps and SDK receivers keep independent acknowledgement state.
- New consumers begin at the deployed version's creation time, including events received before initial discovery. Restarting or promoting a family preserves its durable cursor. Events admitted after promotion use the current version; already admitted executions retain their original version.
- Engine rechecks the current attachment, workspace, and event scope before every execution. Removing the active version or its event selections stops new automatic runs; subscription reconciliation follows within five seconds.
- Event admission and execution-result creation commit together. A repeated event ID for the same family finds its existing admission, including after promotion. Lightweight event fences last 32 days, beyond the existing stream's 30-day retention; private results and diagnostics keep their normal 24-hour retention.
- Infrastructure failures before admission use the existing webhook retry limit and five-second delay. Once admitted, an event owns one execution. Authored failures are retained for inspection and acknowledged; they do not automatically repeat partially completed provider effects. The normal stale-execution recovery marks interrupted runs failed or indeterminate.
- Inspect runs through the app's existing Requests and private diagnostics views. Results include `sourceWebhookEventId`; logical receipts use webhook transport and normal Unified App accounting. Automatic invocations do not create or borrow an execution token.

Fused-owned connected-auth lifecycle events remain on the existing SDK receiver surface. This trigger consumes explicitly attached provider events.
