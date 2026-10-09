---
name: import-service-webhooks
description: Prepare webhook event definitions from an OpenAPI source for an existing Fused service version, with a reviewed import. Use for importing provider webhook schemas, not provisioning receiving URLs or signing secrets.
---

# Import service webhooks

1. Read `get_page_context`. Identify the intended existing service and provider version. Use `service-discovery` to resolve ambiguous service names; navigate only through links returned by page context. Ask the user to open the intended service if no supported link exists. The service page must report `serviceImport.available`; missing ownership, import permission, or an open review cannot be bypassed.
2. Obtain the user's credential-free OpenAPI document or public specification URL containing webhook definitions. For a documentation website, use `source_mode: "docs"` and `source_url`. This runs the shared CLI discovery API, using admitted pages and the Registry model to extract cited JSON POST event schemas. It preserves existing event definitions and verification settings; it does not infer schemas from examples alone. For a specification use `source_mode: "spec"`. Do not fabricate event schemas from unread documentation or turn outbound endpoints into inbound events. Ask for a specification or source content when evidence is missing.
3. Call `prepare_service_import` with the fresh page revision, `target_type: "webhooks"`, and exactly one of `source_url` or `source_content`. The browser supplies the selected service identity and destination version and reads its current revision. Do not supply credentials, signing secrets, receiving URLs, or instructions to fetch authenticated sources.
4. Read the resulting review. The import is limited to the webhook surface and preserves endpoints, but can replace existing webhook definitions and shared webhook settings. Review added, changed and removed counts; name removals when supplied. If the user wanted one added event and the plan removes other events, do not call it additive: ask for a complete source that preserves them and have the user discard the old review before planning again.
5. Explain that the plan is not applied. The user chooses **Apply import** and confirms in the UI. No agent tool may apply the receipt, register a receiving URL, or configure credentials. A version/revision conflict requires a fresh review; **Check status** is available only after an uncertain apply, not during ordinary review.

Importing an event contract and creating a webhook receiver are separate tasks. Use `workspace-drafts` for the receiving-URL form and `credentials-and-access` for credential guidance when those are requested.
