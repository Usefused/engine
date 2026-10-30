# Connect an MCP server with OAuth

Fused MCP servers accept OAuth connections alongside existing execution tokens.
Use the server's stable **Streamable HTTP** URL in Claude, ChatGPT, or another
MCP client. Choose OAuth and automatic client registration when offered. Fused
opens its sign-in page, then asks you to approve specific actions grouped by
service. For services using connected OAuth/OIDC credentials, choose an existing
connected account. Nothing is selected by default.

The authorizing user needs `app.mcp.use` and `app.mcp.tokens.manage` for that
server. Provider accounts must already be connected in the server's bucket.
An account choice is fixed for the connection; a client cannot override it with
headers. API-key and other static credentials continue to resolve inside Fused.

## Client setup

- Configure Engine's public HTTPS URL and browser sign-in before connecting.
- Add the stable Streamable HTTP URL, such as
  `https://engine.example/mcp/<mcp-id>`, to the client.
- Select OAuth with **automatic registration**. Client ID Metadata Documents
  are not advertised by this implementation; choose automatic registration
  rather than a published-identity option if the client offers both.
- Sign in to Fused, select actions and connected accounts, and approve.
- To use a pre-registered client, create one under **Settings → Advanced → OAuth clients**
  with the MCP connection scope (`mcp.execute`) only and the client's exact
  callback URL. Enter the resulting client ID and, for confidential clients,
  secret in the MCP client.
- Disconnect under **Connected apps**, revoke the OAuth client, or revoke the
  issued execution token. Each revokes the relevant grant's future execution;
  an access-token revocation also prevents refresh from restoring that grant.

The stable URL follows explicitly promoted versions. A version-pinned
Streamable HTTP URL also works, but its grant cannot be used at the stable URL
or another version URL. New action names never join an existing grant
automatically. Reconnect and approve a new selection to add actions.

This runtime issuer supports `kind: mcp` servers. Legacy SSE URLs and optional
MCP delivery for SDK/Unified App versions retain their existing token workflow.
Consent includes the version's callable physical operations and imported MCP
capabilities exposed by its operation catalogue. It does not grant unlisted
webhook subscriptions or attached applications.

## Protocol contract

| Purpose | Endpoint |
| --- | --- |
| Protected resource metadata | `/.well-known/oauth-protected-resource/mcp/<id>` |
| Authorization-server metadata | `/.well-known/oauth-authorization-server/mcp/oauth` |
| Issuer | `/mcp/oauth` |
| Authorization | `/mcp/oauth/authorize` |
| Consent POST | `/mcp/oauth/consent` |
| RFC 7591 registration | `/mcp/oauth/register` |
| Code exchange and refresh | `/mcp/oauth/token` |
| RFC 7009 revocation | `/mcp/oauth/revoke` |

Unauthenticated requests receive HTTP 401 with a `WWW-Authenticate` resource
metadata reference. Authorization Code with PKCE S256 is mandatory. Public
clients use `none`; confidential clients can use `client_secret_basic` or
`client_secret_post`. The sole protocol scope is `mcp.execute`; the separately
persisted consent snapshot contains the exact action allowlist and opaque
provider-connection identities. Management OAuth scopes cannot be mixed with
this scope, and runtime OAuth tokens cannot authenticate the management API.

Authorization, code exchange, and refresh requests must include one `resource`
parameter containing the exact canonical MCP URL. The token exchange checks
the resource before consuming the code or refresh token. Tokens cannot be used
at another resource even when its server belongs to the same user.

Dynamic registration initially expires after one hour. Successful browser
consent makes that MCP client durable. Unapproved registrations are bounded to
1,000 live provisional clients across the Engine; each user may consent to at
most ten active dynamic clients. Registration itself grants no execution rights.
The shared OAuth implementation owns token lifetimes, hashing, rotation, code
consumption, and refresh replay detection. Management and MCP connections share
the HTTP authorize, consent, token, and registration handlers, discovery metadata
builder, and branded consent page. MCP adapters add the resource audience and
service/action/account selection; local CLI registration retains its strict
loopback-only request format. Provider credentials never leave the Engine's
existing credential store.

Every OAuth-authenticated execution checks the live grant, client, user status,
and server delegation permissions through the shared runtime authorization
query. OAuth grants bypass the ordinary execution-token authorization cache.
After refreshing a token on an existing Streamable HTTP session, a valid
replacement receives HTTP 404 for the old session so the MCP client can
initialize a new session with the new credential and expiry. Unrelated grants
cannot retire or take over that session.

## Verification

Focused checks:

```sh
go test ./internal/engine/oauthprovider ./internal/engine/auth ./internal/engine/api ./internal/engine/sandbox ./internal/engine/store ./cmd/engine/cmd -run 'OAuth|MCPAuthDiscovery|MCPConsent|MCPTokenHTTP|MCPRefreshSessionOwnership'
```

Set `DATABASE_URL` to an isolated PostgreSQL test database and run:

```sh
go test ./internal/engine/store ./internal/engine/oauthprovider -run 'TestOAuthMCPPostgres|TestMCPSharedCorePostgres' -count=1
```

The database tests cover atomic code exchange, audience mismatch, code replay,
refresh rotation and reuse, management/runtime separation, live permission
removal, account-picker isolation, fixed-account preservation after token cleanup,
manual execution-token revocation, and disconnection. The shared-core integration
test also covers public registration, client promotion, PKCE, and token exchange.
Without `DATABASE_URL`, those integration tests skip; a passing unit-only run
does not establish database or live-client interoperability.

Live ChatGPT verification on 2026-10-30 used an isolated Engine, Registry, and
PostgreSQL database with a temporary public HTTPS endpoint. ChatGPT discovered
the protected resource and OAuth issuer, dynamically registered its client,
used S256 PKCE, displayed the service/action consent page, and exchanged the
authorization code. Selecting only `readEcho` produced a grant containing that
action alone. ChatGPT received the real HTTP echo response, while an attempted
`readRandomId` call using the same sandbox was rejected as an unavailable
operation. Engine execution records independently confirmed successful HTTP 200
responses for the allowed action.

Browser consent must bypass the embedded SPA, preserve the same-origin Origin
header on native form submissions, and use the existing no-store provider
handoff page before navigating to the client callback. Modern MCP structured
results must include tool content alongside `stateHandle`; otherwise clients
that prefer structured results can hide successful provider output.

This live check used a public, credential-free provider fixture. It does not
establish live Claude compatibility, provider connected-account selection,
refresh rotation, cancellation, or revocation behavior.

Before releasing, test real browser consent at desktop and narrow widths, then
connect with Claude and ChatGPT using both automatic and pre-registered clients.
Verify selected and rejected actions, cancellation, refresh/reinitialization,
revocation, server promotion, and removed provider connections. Do not treat
rendered HTML assertions as browser-layout or live-client verification.

Protocol references: [MCP authorization](https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization),
[OpenAI authentication](https://developers.openai.com/plugins/build/auth), and
[Claude custom connectors](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp).
