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
