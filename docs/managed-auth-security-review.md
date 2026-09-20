# Managed auth security review — 2026-09-20

Scope: managed OAuth enrollment, broker exchange and refresh, published app
credentials, authenticated webhook ingress, remote webhook relay, and SDK/MCP
event authorization. This is a focused code review and dependency check, not a
full product penetration test.

## Findings and fixes

Credential-bearing enrollment, refresh, connect and Registry introspection
clients inherited HTTP redirect behavior. A 307/308 response could replay a
refresh token or single-use enrollment ticket to the redirect destination.
The shared managed-auth transport now refuses every redirect, rejects non-HTTPS
destinations except literal loopback addresses for local testing, preserves TLS
verification, and applies bounded timeouts without mutating caller clients.
Regression tests cover every redirect class, same-origin redirects, cleartext
destinations, and the actual broker/connect/Registry client constructors.

Dependency scanning found affected imported packages in Engine, Registry and
CLI. Builds now require Go 1.26.6. Engine and Registry use gRPC 1.83.2,
klauspost/compress 1.18.7, and the networking dependencies required by gRPC;
Registry also updates chi to 5.3.0. Engine's container builder uses Go 1.26.6.
The gRPC updates address the reported [HTTP/2 memory-exhaustion issue](https://pkg.go.dev/vuln/GO-2026-6348)
and [missing-authority panic](https://pkg.go.dev/vuln/GO-2026-6443).

## Verification

- Package-level govulncheck: no affected imported packages in Engine's headless
  command, Registry's command, or CLI.
- Engine regression packages passed: connectauth, managedauthtransport,
  managedauthbroker, managedauthclient, webhookrelay, API, store, sandbox,
  webhookverify, signaturepolicy and Engine command.
- Race-enabled PostgreSQL broker/client/relay tests passed in a new disposable
  database. Tests cover installation isolation, unauthorized token possession,
  subscription quotas, policy changes, disconnect, refresh proof continuity,
  duplicate delivery, forged acknowledgements and cross-installation access.
- Registry API, graph, OpenAPI, shared model/config/signaturepolicy tests passed.
  Race-enabled PostgreSQL tests also passed for one-time ticket redemption,
  installation isolation, and withdrawal of account/license authority.
- CLI command, API and config parser tests passed. Its SDK skill was shortened
  to satisfy the existing 320-line limit without removing managed-service guidance.

## Limits

Symbol-level govulncheck crashed inside its SSA/type-parameter analysis; the
completed dependency scans use package reachability. Engine and Registry still
have seven module-only advisories in unimported SSH/OpenPGP and kin-openapi filter
packages. Those are not reported as affected packages in the scanned commands;
reassess them before introducing those package imports.

The UI audit identified high-severity dependency advisories. Vite is updated to
6.4.3 (including its transitive build copy), js-yaml to 4.3.2, nanoid to 3.3.18,
and toml to 4.2.0. The UI production build and 287 tests passed.

The remaining high audit entries are the Remix/turbo-stream Single Fetch chain.
The [upstream advisory](https://github.com/advisories/GHSA-rxv8-25v2-qmq8) applies
to server-side Single Fetch serialization. This UI sets `ssr: false`, serves
static assets, and now explicitly sets `v3_singleFetch: false`. That affected
server path is not enabled. Do not enable Single Fetch without migrating to a
patched framework version; forcing turbo-stream v3 underneath Remix v2 would
change its serialization API. Low/moderate frontend advisories remain outside
this high-severity review. The raw npm audit is therefore not a zero-findings
report.

Existing live Google/Slack acceptance evidence is recorded separately. The live
Engines, provider credentials, databases and tunnel were preserved; running
processes were not rebuilt or restarted for this review. Security updates take
effect when the reviewed code is rebuilt and deployed.
