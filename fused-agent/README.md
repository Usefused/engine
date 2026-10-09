# Fused workspace agent

This project adapts the Threadify agent implementation for Fused. It uses the same
Harnest agent graph, session history, lifecycle discovery, shared session/checkpoint
store, progressive skills and suspended frontend-tool continuation flow.

The Engine supervisor and private TLS runtime follow Threadify's bundled-agent
implementation. The UI adapts its `AgentSidebar`, tool activity reconciliation,
per-turn route envelope and `HarnestThreadChat` history controls. Fused retains its
responsive docked/sidebar-popup presentation and full-screen mobile chat.

The bundled agent is enabled by default. Operators can disable it with
`engine.ai.enabled: false` in Engine configuration or `FUSED_AGENT_ENABLED=false`.
An explicit environment value overrides YAML. Runtime startup remains independent
of Engine readiness, and model inference uses Registry by default.

## Request flow

1. The browser uses its normal Fused session and CSRF protection to call `/agent`.
2. Engine verifies the workspace actor and proxies only conversation routes to its
   supervised Harnest runtime. An agent-only, short-lived identity replaces the
   browser credential; it cannot authorize general Engine API calls.
3. Harnest loads the Fused agent, project instructions and skill catalog. It owns
   conversation history and checkpoints. Skills load on demand.
4. The model uses Registry's licensed `/agent/v1/chat/completions` gateway by
   default. Registry selects the configured provider/model and supports both
   streaming turns and non-streaming tool continuations.
5. A frontend tool suspends the response. The browser validates the current page
   revision, field visibility and existing API permissions, then submits the
   result to `/agent/client-tools/{id}`. Harnest resumes the same response.

## Fused adaptations

- Replies use visible names or slugs. UUIDs stay in tool arguments and link
  destinations unless the user explicitly asks to see an ID.
- Threadify contracts, graphs and profile tools are replaced by Fused page,
  non-sensitive draft, selected-provider-contract and Unified App compile tools.
- Navigation, non-sensitive draft edits and validation/compilation run directly.
  Saving, submitting, creating, deleting and deploying remain manual user actions.
  The transport supports Harnest human approval for future protected tools; it
  never treats an approval as permission to invoke an undeclared frontend action.
- Secret values remain hidden and uneditable. There is no general API execution,
  submit, deploy, provider-execution or permission-changing tool.
- `lib/identity.py` verifies Engine-issued identities using Fused's actor/workspace
  scope; Threadify-specific bearer introspection and GraphQL clients are not used.
- `lib/ai_gateway.py` uses Engine-resolved Registry/custom-gateway settings and
  isolates credentials and connections per model. It does not read other Engine
  configuration or ambient provider credentials.
- As in Threadify's bundled mode, session/checkpoint storage is in memory and
  resets when the child runtime restarts. `lib/storage.py` also preserves the
  upstream standalone `HARNEST_DATABASE_URL` store option; the bundled supervisor
  deliberately does not pass Engine database credentials to the child.

Compile with `runtime/build.py --compile-only --python <python3.12> --output <dir>`.
Compilation uses inert gateway placeholders; runtime settings are supplied by
Engine at launch. The portable-runtime integration test exercises skill discovery,
loading, frontend reads/edits, continuation, follow-ups and cross-actor isolation.

Release CI stages verified agent inputs into the ignored
`internal/agentbundle/release-assets/` directory. GoReleaser builds both Engine
variants with `fused_agent_release` to embed those inputs; missing staging fails
compilation. Ordinary local builds use the checked-in `assets/` bundle. This keeps
release generation from modifying tracked files before GoReleaser's Git checks.

The agent can progressively discover catalogue services (`search_services`), browse
versions and endpoints (`search_service_operations`), and inspect exact request and
response contracts (`read_service_contract`). These reads use the signed-in user's
existing Registry authorization; they never read bucket secrets or invoke providers.

For an existing Unified App, `revise_unified_app` reuses Describe's intent,
operation-selection and source-revision APIs. It adds missing capabilities and
updates TypeScript together in the unsaved editor, preserving existing version pins,
auth selection and app settings. Failed, cancelled or stale revisions leave the
draft unchanged. Saving and deployment remain manual.

For a deterministic browser check, build the UI and run
`node ui/scripts/fused-agent-revision-fixture.mjs`, then open
`http://127.0.0.1:18210/integrations/unified-apps/new?edit=fixture-checkout`.
The real editor/sidebar run against synthetic catalogue, model and compile responses;
no app is deployed. Ask to find a customer by email to exercise discovery and revision.
Requests containing `fail`, `slow` or `stale` exercise failure, cancellation or stale
context respectively. `/fixture/report` records the synthetic call sequence.

`set_unified_app_view` lets the agent open the TypeScript or YAML tab with a fresh
page revision. YAML configuration and credential references are readable and editable
through the existing visible-form tools. Syntax/config errors are returned as failed
edits, remain visible for correction, and block compilation. Actual credential values
stay in buckets. Both editor views share one unsaved draft; neither tool saves or
publishes it. In the browser fixture, messages beginning with `config` exercise YAML
editing; add `invalid` to check validation feedback.

### Existing-service imports

The `import-service-webhooks` and `import-service-endpoints` skills use
`prepare_service_import` on an owned service details page. The browser binds the
selected service/version and calls the ordinary import plan API; the model cannot
choose another destination or apply the result. The review shows additions,
changes and removals, and uses the shared confirmation prompt for manual apply.
Uncertain outcomes use import status recovery. Both skills accept specification URLs or documents. Webhook imports also accept a documentation website with `source_mode: docs`: the shared Registry discovery API extracts cited JSON POST event schemas, preserves existing events and verification settings, and returns a review.
Webhook definition import does not provision receiving URLs or signing secrets.
