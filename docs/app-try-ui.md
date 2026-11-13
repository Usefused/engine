# Shared Try sidebar

Unified Apps and REST API app details launch the same sidebar. It reuses Fused’s operation inspector shell, searchable selectors, input chips, JSON editor, and button styles. The inspector supports keyboard dismissal, focus restoration, independent scrolling, and narrow viewports.

The public OpenAPI document for the exact immutable app version supplies the operation picker, input schema, and permitted runtime buckets. Unified Apps show their authored `execute` input; REST APIs show the selected operation’s input. Required fields and schema constraints validate before execution, with JSON available for complex shapes. Selecting another operation clears input, credentials, routing, and output. Closing the sidebar clears the session.

## Routing and readiness

Configured routing preserves the default and per-service buckets. An explicit bucket must appear in the deployed version’s finite runtime allowlist. The sidebar never edits bindings or expands that allowlist. Versions without runtime selection remain runnable with configured routing.

`GET /apps/{app_id}/try-readiness?operation=…&bucket_id=…&end_user_ref=…` inspects the immutable operation’s required authentication and the selected bucket’s current configuration. It requires app read access, checks credential metadata and connection permissions independently, returns no secret values, and uses `Cache-Control: no-store`. Missing permissions show unavailable status instead of claiming a connection is ready. OAuth checks distinguish absent user references, missing grants, refreshable grants, and reconnect-required grants through one metadata query. Managed auth is labeled separately.

Readiness is advisory: provider access, scopes, and resource routing are checked during execution. Unified App code determines its connected users from authored input, so static inspection cannot promise user consent readiness. Child apps keep their own buckets. Readiness for nonphysical REST aliases is reported as unavailable. Bucket setup opens separately; refresh rechecks after configuration changes. Selection changes abort outstanding inspections and hide stale results immediately.

## Execution

By default, clicking Run uses the signed-in user’s existing `app.<kind>.tokens.manage` permission to generate a five-minute execution token scoped to the selected operation. The ordinary issuance API remains authoritative; app read access alone cannot issue a token. No token is issued just by opening Try. Completed synchronous requests revoke their generated token; pending or uncertain requests retain the token only while the sidebar stays open. Closing the sidebar, changing operations, or leaving the page requests revocation immediately, even if transport has not settled. Closing during issuance cleans up the returned token without executing. A failed cleanup still leaves the five-minute expiry in effect.

**Use execution token** optionally accepts a caller-supplied token. It bypasses automatic issuance and preserves the token’s runtime scope. Supplied tokens are never revoked by Try. All token values stay in component memory; supplied values are cleared after each attempt or mode switch. Runtime requests always use bearer authentication and omit management cookies. Requests are never automatically retried. HTTP status, elapsed time, and the response body remain visible, with a link to existing Requests activity. Transport failure explicitly leaves the execution outcome uncertain.

Unified Apps use the stable family URL with an `expected_app_id` fence, so promotion cannot silently run a different input contract. REST API calls target the exact version. Runtime bucket overrides reuse live deployed-scope authorization before dispatch and credential resolution.

## Local verification

From `ui/`, run `node scripts/try-ui-fixture.mjs` and open `http://127.0.0.1:18213/` for REST APIs or append `?unified` for a Unified App. The fixture renders production components with synthetic schemas, readiness, and execution responses; it makes no provider calls.

Regression coverage includes contract references and validation, denied bucket overrides, metadata permission isolation, per-service routing, stale readiness responses, duplicate-submit prevention, token clearing, and operation switches. The PostgreSQL connection test uses the repository’s isolated test schema when `DATABASE_URL` is configured.
