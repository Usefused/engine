---
name: unified-app-editing
description: Discover missing service operations, revise Unified App configuration and TypeScript together, and compile the unsaved draft.
---

# Unified App editing

1. Read get_page_context for the current source, revision and connected editor. A Unified App is a deterministic integration capability callable by agents and backends.
2. For a new capability, missing operation or service, or changed business logic, call `revise_unified_app` with the user's requested change and current page revision. It uses the same APIs as Describe, discovers exact missing operations, retains existing versions and aliases, and revises TypeScript with the expanded selection as one unsaved draft. Do not ask the user to add an operation manually when this action is available. Do not invent operation names or replace the app using the creation tool.
3. Read the resulting page and use read_selected_contracts to inspect exact selected versions and operation input/output shapes before additional manual source changes. Read more contract pages when necessary.
4. Distinguish app-source errors from Engine serialization or credential errors. Preserve provider-declared arrays and objects; do not invent bracketed payloads to hide a serializer bug.
5. Small source-only corrections can use update_form_field with the complete corrected source and current field handle/revision. Preserve unrelated user code and behaviour. Configuration changes from revise_unified_app appear in the YAML view automatically.
6. Re-read the draft and call compile_unified_app with its current revision. Repair at most twice without discarding requirements. Compilation does not execute the provider or prove credentials are configured.
7. Explain the confirmed edit and compiler result concisely. The user reviews and saves/deploys the draft. If an Engine defect blocks progress, say exactly what evidence establishes that limit rather than claiming the app was fixed.

## YAML configuration

Use `set_unified_app_view` with `view: yaml` and the current page revision to read
and edit the app configuration. Its auth scheme names, bucket references, operations
and other settings are ordinary configuration, not secret values. Read the returned
page, then use `update_form_field` with the YAML field handle, complete edited YAML
and fresh revision. Preserve unrelated settings. Inspect validation errors and repair
them before compiling or switching back with `view: typescript`. Do not claim a
rejected edit was applied. New providers or versions still require grounded discovery
through `revise_unified_app` in the TypeScript view; never guess their identities.
Credentials remain in buckets; never put secret values into YAML. Saving and
publishing remain manual.
