---
name: unified-app-creation
description: Create an unsaved Unified App from a goal using Fused's existing Describe APIs, then review and compile it in the active editor.
---

# Unified App creation

1. Read `get_page_context`. Open the Unified App creation page through an available page link if needed. The page must report `canDescribeUnifiedApp`.
2. Gather the user's goal, intended services and necessary business choices. Use `describe_unified_app` with that goal. This calls the same shared Describe pipeline: intent parsing, service discovery, exact operation/event selection and contract-grounded TypeScript generation. Do not substitute invented operations or hand-written API requests.
3. If discovery reports ambiguous publishers or missing details, ask a concise follow-up and retry with the user's explicit choice. Do not guess from identifiers. Preserve valid selections on a retry.
4. Read the resulting page and its TypeScript. The result is an unsaved draft in the normal editor. Inspect `read_selected_contracts` before changing input/output mappings. Use the Unified App editing skill for follow-up changes; do not regenerate the whole app for a small edit.
5. Complete visible non-sensitive draft fields using `update_form_field` and a current revision. Ask the user to select or configure credentials themselves. Never read or write secrets.
6. Run `compile_unified_app` with the current revision when the editor is ready. Report actual compilation results; compilation neither executes providers nor verifies their credentials. If an inactive service blocks compilation, tell the user what must be enabled rather than making a persistent change.
7. Summarize the draft and validation result briefly. Saving, deployment, service activation and token issuance remain manual UI actions. Do not claim a deployed app or successful provider execution.
