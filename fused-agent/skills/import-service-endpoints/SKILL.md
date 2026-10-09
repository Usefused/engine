---
name: import-service-endpoints
description: Prepare an endpoint specification import into an existing Fused service version, reviewing additions, changes and removals. Use for adding or updating provider endpoint contracts, not selecting operations in a Unified App or executing a provider call.
---

# Import service endpoints

1. Read `get_page_context` and confirm the existing service, publisher and selected version. Use `service-discovery` to inspect existing operations and contracts if needed. Follow only returned page links; if the intended service has no supported link, ask the user to open it. Require `serviceImport.available`; do not create a replacement service to bypass ownership or import permissions.
2. Use a credential-free machine-readable specification supplied by the user or its public URL. Preserve exact provider methods, paths, auth scheme declarations, parameter serialization, request and response shapes. Do not invent schemas from operation names or rewrite valid arrays to work around Engine errors. Documentation pages require the separate discovery workflow; `prepare_service_import` does not crawl them. Ask for the specification or source content when that is all the current tools can import.
3. Call `prepare_service_import` with a fresh page revision, `target_type: "endpoints"`, and exactly one of `source_url` or `source_content`. It uses the same `/integrations/import/plan` endpoint as `fused-cli import plan`. The browser binds the service and selected version; a source declaring another version is rejected rather than silently creating or switching versions.
4. Inspect the returned diff and rendered review. Endpoint imports replace the selected endpoint contract, not an append-only list. A partial specification can remove existing endpoints. If the request was to add endpoints, preserve the existing contract in the supplied source and reject unintended removals; do not claim the importer merges them. Keep webhook changes out of this skill.
5. Report the planned counts and relevant removals, and state that nothing has been applied. The user reviews **Apply import** and its confirmation. Do not submit, invoke providers, activate services, or store credentials. Resolve uncertain outcomes using the review's **Check status** control before another import.

Selecting an already imported operation for an app is a different task: use `unified-app-editing` for `revise_unified_app`, or `sdk-mcp-guidance` for those app types.
