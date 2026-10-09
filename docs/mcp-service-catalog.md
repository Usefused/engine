# Imported MCP catalogs

The service details MCP tab imports one remote server catalog per actor and enabled
service version. The UI supports discovery, review, import, refresh, metadata
search, resource-type filtering, and expandable definitions for tools, prompts, resources, and resource
templates in collapsible endpoint-style groups with type badges. Selecting an item
opens the shared operation inspector style: tool input/output schemas, prompt
arguments, resource URIs and content types, and the original definition. Schema
references stay local to the imported document; inspection never fetches resources
or executes tools. Search fields fill their catalog toolbar, and Operations has an
exact method filter that retains pagination and selected resource versions. Opening the tab reads the saved snapshot without contacting the server.

Catalog attachments remain Engine-local and separate from Registry contracts.
Importing or refreshing a service catalog does not change existing apps. A new MCP
app version can explicitly select tools, prompts, resources, and resource templates
from an approved catalog revision. The app stores those exact definitions and its
connection reference in the existing immutable selection contract.

## Connection and access

Remote Streamable HTTP over public HTTPS is supported through the official Go
MCP SDK v1.8.0, including negotiation with older supported protocol revisions.
URLs containing user info, query strings, or fragments are rejected. Local stdio
servers, legacy SSE endpoints, private network targets, and interactive OAuth
onboarding are not supported by this initial catalog importer.

Operations, Webhooks, and MCP share a section heading, description, Options
dropdown, and separate search row. Owner-only endpoint and webhook imports live
in their respective tab menus; the service header’s More actions menu links to
service configuration and workspace membership actions. MCP Options offer Import MCP server before
attachment, then Change server URL and Refresh catalog. URL changes are discovered
and reviewed before replacing the saved catalog. The service is the sole display
identity; provider handshake names and versions are not shown as a separate server.
All mutation entries require confirmed Registry
service ownership and catalogue-import permission; route shape never implies ownership.
MCP discovery and apply also require service management access and recheck
Registry ownership before contacting a provider or promoting a draft. Bearer authentication uses
an existing generic bucket secret, selected by bucket name and secret name.
Choosing a token destination additionally requires workspace credential-management
and bucket-use permissions. Tokens remain in Engine and are not saved in drafts.
Catalog reads require service-read access; bucket-derived metadata also requires
current bucket-use access. Each catalog is private to its importing actor.

## Review and storage

The version-scoped control routes are:

- `GET /workspace/services/{id}/versions/{version_id}/mcp-catalog`
- `POST /workspace/services/{id}/versions/{version_id}/mcp-catalog/discover`
- `POST /workspace/services/{id}/versions/{version_id}/mcp-catalog/apply`

Discovery accepts `url` and optional `bucket_name` / `secret_name`. Apply accepts
only a server-issued `draft_id`, never browser-supplied definitions. A preview
expires after 15 minutes and pins the previously approved revision. Concurrent
changes cause a conflict requiring new discovery. Applying the current approved
draft again is idempotent. An expired or failed discovery leaves saved state intact.

Engine startup creates the catalog head and revision tables. Workspace-version
removal cascades to its catalog records. There is one pending preview per actor
and version. The transport restricts responses to 2 MiB, complete catalogs to
8 MiB, each list to 2,000 entries and 50 pages, and discovery to 30 seconds.
Redirects and private/reserved DNS answers are rejected; DNS resolution is pinned
to the admitted address. Provider descriptions and resource URIs render as inert text.

## Verification

Automated coverage includes real SDK discovery against a local MCP fixture using
2025-11-25 and 2026-07-28, all four lists, pagination limits, duplicate identities,
JSON Schema keyword retention, safe errors, credential authorization and isolation,
response limits, and real PostgreSQL persistence/expiry/concurrent promotion.

The manual browser fixture mounts the production `McpCatalogTab` component with
real Engine HTTP handlers and PostgreSQL persistence. Provider catalogs are
synthetic, so the fixture never transmits workspace credentials. Enable
`TestMCPCatalogBrowserPreview` with `FUSED_MCP_UI_DIR` pointing to the bundled
`ui/mcp-catalog-preview.tsx` page and `DATABASE_URL` pointing to a disposable
PostgreSQL instance on `127.0.0.1:55489`. It listens only on `127.0.0.1:4179`.
The ordinary test suite skips this manual listener.

Browser checks completed: empty state, discovery preview, endpoint-style catalog rows, type filtering, combined search and type filters,
import, saved-state reload, URI search, tool schema expansion, unchanged refresh,
discard, failed discovery retaining saved state, and unsupported capabilities.
The full Fused service page was also checked with a separate feature Engine and
a copy of the newest local test workspace: Demo CRM, real public DeepWiki
discovery/import, type filtering, and persisted reload. The service-native Options
dropdown, saved URL editing/cancellation, reviewed Save changes, and absence of
repeat import actions were also verified on that full service page. The shared
Operations/Webhooks/MCP headers, tab-local endpoint import drawer, and top-level
configuration shortcut were checked in the browser. The local Registry fixture
does not supply the webhook editor source payload, so that editor was checked
only for relocation and opening, not a complete import. Production OAuth was not tested.

The affected Go packages, PostgreSQL race test, new UI tests, component lint, and
production UI build passed. The full UI test suite retains 16 failures and the
TypeScript check retains three unrelated errors reproduced on the unchanged base
revision (77defec); the touched service identity narrowing error was corrected.
The endpoint resource renderer was split into smaller presentation helpers while
adding method filtering; all affected UI components now pass lint.

Additional browser checks passed for GET/POST filtering, combined operation search
and method filtering, widened searches in all three tabs, grouped MCP search,
tool inspector schema expansion, and Escape restoring focus to the selected row.
The focused UI tests pass (19 total); three unrelated baseline TypeScript errors remain.

## Using imported capabilities in MCP apps

The MCP app builder renders a collapsible MCP section beside Endpoints and Webhooks,
with explicit Select All, search, resource-type filtering, and grouped rows. Its
selection pins `mcp.revision_id` and exact `tools`, `prompts`, `resources`, and
`resource_templates` arrays in the service entry. Resources are selected by URI;
templates by URI template. Empty, stale, duplicate, or unknown selections fail plan.
Imported capabilities are currently admitted only for `kind: mcp`.

Selected tools appear in `search_docs` with their original input schema and execute
through `execute` using `call("mcp:<service-id>:tool:<name>", args)`. Fused still
exposes exactly two tools. Native `prompts/list` / `prompts/get` and
`resources/list` / `resources/templates/list` / `resources/read` expose selected
prompts and resources directly. Prompt names and resource URIs are service-qualified
to prevent collisions. Resource templates grant only matching concrete URIs.

Family-token restrictions apply to discovery and invocation using the same exact
capability grants. Adding imported capabilities participates in normal immutable
versioning, capability hashes, and restricted-token expansion checks. Connections
use the service's app-family bucket; credential-derived catalogs require the same
bucket used at import. Provider credentials remain in Engine. Calls use the normal
account quota/concurrency gate and canonical execution-event publisher. Activity
metadata excludes arguments, results, credentials, and concrete resource URIs.

Upstream invocation uses the same public-HTTPS and DNS restrictions as discovery,
a 30-second budget, bounded results, and no automatic retries. Tool arguments are
validated against the pinned schema with no remote reference fetching. Upstream
change notifications, subscriptions, interactive OAuth, and bidirectional provider
requests are not forwarded. Catalog refresh requires an explicit new app selection
to expose changed definitions.

Verification includes real SDK tool/prompt/resource calls for both supported
protocol revisions, exact selection and token isolation, schema validation,
resource-template admission, URI namespacing, immutable pinning, and PostgreSQL
restricted-token expansion checks. The local Fused UI also created an imported-only
MCP app and called its selected public DeepWiki tool through `search_docs`/`execute`.
