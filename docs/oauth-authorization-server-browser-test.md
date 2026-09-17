# OAuth authorization server browser test

This test verifies the complete third-party OAuth2 authorization-code + PKCE
flow through Engine's own authorization server: client registration, the
server-rendered consent screen, code/token exchange, refresh rotation, the
end-user "Connected Apps" self-service page, and the security-relevant denial
and revocation paths. Use a disposable workspace or clean up created OAuth
clients afterward.

Never print raw client secrets, authorization codes, access tokens, refresh
tokens, PKCE verifiers, or their hashes while collecting browser, terminal, or
telemetry evidence. Redact them in any pasted output.

## Prerequisites

- Engine HTTP and its embedded UI are running, with an admin browser session
  that holds `access.manage` (for registering clients) and a second, distinct
  end-user browser session that holds at least `service.read` (for the
  consent/grant walkthrough).
- A terminal HTTP client (`curl`) for the token/refresh/revoke exchanges,
  which are not browser UI surfaces.
- A reachable HTTPS (or `http://localhost`) redirect URI you control enough to
  read the `code`/`state`/`error` query parameters Engine appends to it (a
  local static page or a request-inspection tool is sufficient).
- `openssl` or an equivalent to compute a PKCE `S256` code_challenge from a
  code_verifier: `printf '%s' "$VERIFIER" | openssl dgst -sha256 -binary | \
  openssl base64 | tr '+/' '-_' | tr -d '='`.

## Register an OAuth client (admin)

1. Sign in as the admin user and open **Access → OAuth Clients**
   (`/integrations/access/oauth-clients`).
2. Under **Register a client**, fill in a client name, choose **Confidential
   (server-side app, gets a secret)**, enter your redirect URI under
   **Redirect URIs (one per line)**, and enter `service.read` under **Allowed
   scopes (one per line)**. Select **Register client**.
3. Confirm the amber notice "Copy this client secret now. It will not be shown
   again." appears with the raw secret, and that reloading the page never
   shows it again. Select **I've saved it** to dismiss it.
4. Confirm the new row appears under **Registered clients** showing
   `Confidential · <client_id> · Active` and `Scopes: service.read`.
5. Repeat registration choosing **Public (native/SPA, PKCE only)** and confirm
   the notice instead reads "This is a public client. It authenticates with
   PKCE only and has no secret." with no secret displayed anywhere.
6. Sign in as a user without `access.manage` and confirm the **Register a
   client** form is not rendered, while the registered-clients list (read via
   `access.read`) still is.

## Full authorization-code + PKCE grant (consent screen)

Perform this as the second, end-user browser session, using the confidential
client from above.

1. Generate a code_verifier (43-128 unreserved characters) and derive its
   S256 code_challenge per the prerequisites.
2. Navigate the browser to `/oauth/authorize` with `response_type=code`,
   `client_id=<client_id>`, `redirect_uri=<your redirect URI>`,
   `scope=service.read`, a random `state`, your `code_challenge`, and
   `code_challenge_method=S256`.
3. If not already signed in, confirm Engine redirects to the login page first,
   then returns to the same authorize request after sign-in.
4. Confirm the consent screen renders using the workspace's hosted-connect
   branding (name/logo/color), the heading "`<Client name>` wants to access
   your account", and lists "View connected services" under "This application
   will be able to:" (the human-readable label for `service.read`).
5. Select **Deny**. Confirm the browser lands back on your redirect URI with
   `error=access_denied` and the original `state`, and that no `code` is
   present.
6. Repeat from step 2 and this time select **Allow**. Confirm the browser
   lands on your redirect URI with a `code` and the same `state` you sent.
7. Repeat the authorize request a third time (same client, same scope).
   Confirm Engine now skips the consent screen entirely and redirects straight
   to a fresh `code`, since consent for this exact scope was already granted.

## Exchange the code and rotate the refresh token

Do the following with `curl` using the client secret and the values captured
above (all one-time-use — capture fresh values if you re-run a step).

1. Exchange the code:
   `curl -s -u <client_id>:<client_secret> -d grant_type=authorization_code \
   -d code=<code> -d redirect_uri=<redirect_uri> -d code_verifier=<verifier> \
   <engine-url>/oauth/token`. Confirm the JSON response has `access_token`,
   `refresh_token`, `token_type: "Bearer"`, an `expires_in`, and
   `scope: "service.read"`.
2. Immediately repeat the exact same request. Confirm it now fails with
   `400` and `{"error":"invalid_grant"}` — the code is single-use.
3. Repeat step 1's original successful request pattern with a *new* code but
   the wrong `redirect_uri`, and separately with a mismatched
   `code_verifier`. Confirm both are rejected with `invalid_grant` and that a
   subsequent correct exchange of that same fresh code still succeeds
   (a failed attempt must not burn the code).
4. Call `/oauth/token` with `grant_type=refresh_token` and the `refresh_token`
   from step 1. Confirm a new `access_token`/`refresh_token` pair is returned.
5. Replay the original (now-rotated-away) `refresh_token` from step 1 again.
   Confirm it fails with `invalid_grant`, and that the refresh token issued in
   step 4 *also* now fails if you try to rotate it — reuse of a consumed
   refresh token revokes the entire rotation family, not just the reused
   token.
6. Confirm the access token from step 1 no longer authenticates any
   authenticated Engine endpoint once its family has been revoked (e.g. call
   `/oauth/connected-apps` with it as a Bearer token and expect `401`).

## Connected Apps (end-user self-service)

Still as the end-user session from above:

1. Open **Access → Connected Apps** (`/integrations/access/connected-apps`,
   reachable by any signed-in user — no `access.read` gate).
2. Confirm the authorized client appears with its scopes ("Scopes:
   service.read") and a "Connected `<timestamp>`" line.
3. Select **Disconnect**, confirm the browser prompt reads `Disconnect
   "<client name>"? It will immediately lose access to your account.`, and
   confirm the app disappears from the list after confirming.
4. Confirm any access/refresh token previously issued to that client for this
   user now fails at `/oauth/token` (refresh) and against an authenticated
   endpoint (access token), and that re-running the authorize flow now shows
   the consent screen again (consent was actually removed, not just hidden).

## Revocation and error paths

1. As the admin, revoke the client from **Access → OAuth Clients** (confirm
   prompt: `Revoke "<client name>"? Every token it has issued will stop
   working immediately.`). Confirm every still-live token issued under that
   client immediately stops authenticating, and that `/oauth/authorize` for
   that `client_id` now renders the generic error page instead of a redirect.
2. Request `/oauth/authorize` with an unknown or mistyped `client_id`, and
   separately with a valid `client_id` but a `redirect_uri` that was never
   registered. Confirm both render Engine's own error page ("Connection
   request could not be completed") rather than redirecting anywhere — an
   unverified redirect target must never be used, per RFC 6749 §4.1.2.1.
3. Request `/oauth/authorize` with a `scope` value the client's registration
   does not include in its allowed scopes. Confirm the request is denied
   before reaching the consent screen.
4. As a signed-in user who does not personally hold a requested scope's
   permission (even though the client is allowed to request it), confirm
   `/oauth/authorize` is denied — a client's allowed scopes are a ceiling, not
   a grant, and the authorizing user's own live permissions are the other
   ceiling.
5. Call `POST /oauth/revoke` with a valid access or refresh token, then call
   it again with the exact same value. Confirm both calls return `200` with
   no body distinguishing "revoked" from "already gone / never existed" (RFC
   7009), and that the token no longer authenticates after the first call.
6. Fetch `/.well-known/oauth-authorization-server` with no credentials at all
   and confirm it returns discovery metadata (not a 401).

## Pass criteria

- A client secret is shown exactly once at registration and never
  retrievable again through the UI or API.
- The consent screen renders workspace branding, lists only the requested
  scope's human-readable label(s), and previously-granted-at-or-above scope
  is never re-prompted.
- A denied consent, an unregistered client/redirect_uri, a client-disallowed
  scope, and a user-permission-exceeding scope are all rejected before any
  code is issued; only scope/access-denied errors ever redirect back to the
  client, everything else renders Engine's own error page.
- Authorization codes are single-use and reject wrong redirect_uri/PKCE
  verifier without being consumed by the failed attempt.
- A reused (replayed) refresh token denies the request AND revokes every
  token in its rotation family, including ones already rotated to.
- Revoking a client or disconnecting a connected app immediately invalidates
  every live token it issued, not merely future requests.
- `/oauth/revoke` and `/oauth/connected-apps` disconnect are both idempotent
  and never reveal whether a token/consent existed beyond the first call.
- No client secret, authorization code, access token, refresh token, or PKCE
  verifier appears in server logs, audit records, or browser-visible error
  text.
