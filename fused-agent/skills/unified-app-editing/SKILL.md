---
name: unified-app-editing
description: Diagnose, revise and compile the current Fused Unified App TypeScript draft against its selected provider contracts.
---

# Unified App editing

1. Read get_page_context for the current source, revision and connected editor. A Unified App is a deterministic integration capability callable by agents and backends.
2. Use read_selected_contracts to inspect the exact selected service versions and operation input/output shapes before changing provider mappings. Read additional pages of a contract when necessary.
3. Distinguish app-source errors from Engine serialization or credential errors. Preserve provider-declared arrays and objects; do not invent bracketed payloads to hide a serializer bug.
4. When an app change is justified, apply the complete corrected source through update_form_field using its current field handle and revision. Preserve unrelated user code and requested behaviour.
5. Re-read the draft and call compile_unified_app with its current revision. Repair at most twice without discarding requirements. Compilation does not execute the provider or prove credentials are configured.
6. Explain the confirmed edit and compiler result concisely. The user reviews and saves/deploys the draft. If an Engine defect blocks progress, say exactly what evidence establishes that limit rather than claiming the app was fixed.
