# App permissions by type

Fused separates app authority by delivery type. An OAuth token with `app.mcp.create` can create MCP servers; it cannot create generated SDKs, REST APIs, or webhook ingress registrations.

| Namespace | Actions |
| --- | --- |
| `app.sdk` | `create`, `read`, `use`, `manage`, `tokens.manage` |
| `app.mcp` | `create`, `read`, `use`, `manage`, `tokens.manage` |
| `app.api` | `create`, `read`, `use`, `manage`, `tokens.manage` |
| `app.webhook` | `create`, `read`, `manage` |

Creation requires a workspace grant. Existing SDK, MCP, and REST API actions retain their stable app-family resource boundary. Webhook configuration retains its existing workspace management boundary; this change does not add webhook app families, runtime tokens, or a new webhook listing API. Existing service webhook catalogue discovery remains governed by its service permissions.

The SDK adapter supports both generated packages and REST delivery. `kind: sdk` with `generate: false` requires `app.api.create`. Existing resources use stored family kind and delivery mode; saved-plan apply derives creation authority from the stored plan. Client-supplied labels cannot override either identity.

## OAuth and effective grants

Consent requires an explicit nonempty scope. Effective authority is bounded by the registered client scopes, requested consent, and the user's current resource grants. Owner privileges do not enlarge a narrow client request. The OAuth catalogue and consent descriptions expose the concrete scope names.

Authorization snapshots are cached by credential ID and authorization revision. Two OAuth clients for one user cannot reuse each other's effective permission snapshot. Revocation, expiry, and revision invalidation retain their existing checks.

Shared app collections resolve typed grants to eligible family IDs before pagination and totals. A narrow read grant can return an empty list. Internal shared-route templates such as `app.read` are resolved against persisted identity; these strings are not accepted as external grants or OAuth scopes. Read-only builder selectors can accept any concrete creation grant, while mutations always require their own type.

CLI plan requirements and permission errors retain the exact identifiers. UI creation options, direct builder links, family management, activity access, and resource discovery use concrete permissions. Existing restrictions on delegated clients accessing protected credentials or access-management operations still apply.

## Upgrade behavior

1. Deploy the Engine, CLI, and UI changes together. Normal bootstrap reconciliation updates built-in role permissions and authorization revision. Owner, Admin, and Builder retain their intended capabilities through explicit typed grants. App sharing roles remain bounded to the selected family.
2. Recreate OAuth client registrations that used generic app scopes with the specific types and actions they need. Update authorization requests to send an explicit `scope`, then obtain fresh consent.
3. Access tokens containing retired `app.read`, `app.use`, `app.create`, `app.manage`, or `app.tokens.manage` fail authentication. Authorization-code and refresh exchanges cannot reissue those scopes. They are never silently expanded into the new namespaces.
4. Re-plan pending configurations created before the upgrade. Old generic creation requirements fail closed, and team-owner preflight rejects retired requirements. New plans record concrete permissions and remain subject to revision and ownership validation.

The local implementation does not modify production permissions or deploy these changes.

## Verification

Regression coverage exercises all 16 grant/target combinations for creation through plan and saved-plan apply, explicit OAuth consent, both credential-cache ordering directions, and narrow reads of empty catalogues. PostgreSQL integration coverage authenticates a real MCP-scoped OAuth token, warms the Owner credential, and verifies the narrow token's family reads, management, token management, creation, mixed catalogue rows, and totals. Known foreign family IDs and retired token scopes are rejected.

Local validation: access-control, OAuth provider, Engine API, command middleware, full PostgreSQL store suite, CLI API/command tests, 287 UI tests, and the UI production build passed. Standalone UI TypeScript checking remains blocked by eight unrelated errors in existing extraction, integration selection, connected-app, and app-list code.

Live browser and CLI verification also exercised the scope picker, MCP-only consent and token exchange, typed creation menus, direct-link denials, filtered catalogues, and structured CLI plan denials in a disposable local control-plane fixture. The builder uses the shared workspace permission gate for loading, failure and access-denied states. See `work/permission-browser-20260919/README.md` in the parent workspace for the sanitized evidence and test limitations.
