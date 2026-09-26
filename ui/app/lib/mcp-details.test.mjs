import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

const list = source("../routes/integrations.sdks._index.tsx");
const legacyList = source("../routes/integrations.mcp.tsx");
const detail = source("../routes/integrations.mcp_.$id.tsx");
const sdkDetail = source("../routes/integrations.sdks.$id.tsx");
const detailChrome = source("../components/apps/AppDetailChrome.tsx");
const detailBody = source("../components/apps/AppDetailBody.tsx");
const transportEndpoints = source("../components/mcp/McpTransportEndpoints.tsx");
const activity = source("../routes/integrations.mcp_.$id.analytics.tsx");
const services = source("../components/apps/AppConnectedServices.tsx");
const layout = source("../routes/integrations.tsx");

test("MCP catalogue shares the Apps family list and opens exact MCP details", () => {
  assert.doesNotMatch(list, /APP_CATALOGUE_TABS|aria-label="App type"/);
  assert.match(list, /appFamilies\(search: \$search/);
  assert.match(list, /if \(app\.target_type === "mcp"\) return `\/integrations\/mcp\/\$\{appId\}`/);
  assert.match(list, /delivery_mode/);
  assert.match(list, /deprecateApp\(app_id: \$appId/);
  assert.match(list, /undeprecateApp\(app_id: \$appId/);
  assert.match(list, /<SdkPagination page=\{page\} total=\{total\}/);
  assert.match(legacyList, /next\.delete\("tab"\)/);
  assert.match(legacyList, /<Navigate to=\{`\/integrations\/sdks\$\{suffix/);
  assert.doesNotMatch(list, /<McpTransportEndpoints/);
});

test("MCP details show Engine-projected transports and immutable selected operations", () => {
  assert.match(detail, /app\(app_id: \$appId\)/);
  assert.match(detail, /appServices\(app_id: \$appId\)/);
  assert.match(detail, /operation_names/);
  assert.match(detail, /result\.app\.kind !== "mcp"/);
  assert.match(detail, /<McpTransportEndpoints endpoints=\{server\}/);
  assert.match(detail, /<AppOverviewBody[\s\S]*selections=\{server\.detailed_selections\}/);
  assert.match(detailBody, /<AppConnectedServices selections=\{selections\}/);
  assert.match(detail, /A reusable interface for the services and operations this MCP server exposes\./);
  assert.match(detail, /Copy server URL/);
  assert.match(detail, /versioned_streamable_http/);
  assert.match(detail, /transport_urls \{ versioned_streamable_http versioned_sse \}/);
  assert.match(detail, /<McpVersionTransportEndpoints/);
  assert.match(detail, /label: "Changes"/);
  assert.doesNotMatch(detail, /TerminalSquare/);
  assert.doesNotMatch(detail, /server\.description/);
  assert.doesNotMatch(transportEndpoints, /<PinnedEndpoint/);
  assert.match(transportEndpoints, /SSE · Stable/);
  assert.match(transportEndpoints, /SSE · Version-pinned/);
  assert.match(transportEndpoints, /<TransportBadge legacy>Legacy<\/TransportBadge>/);
  assert.match(services, /appSelectionDisplayRows\(selection\.endpoint_ids, selection\.operation_names\)/);
  assert.doesNotMatch(services, /api\.|mcpGraphql|Registry/);
});

test("MCP Activity is a permission-gated detail tab and the old URL redirects", () => {
  assert.match(detail, /if \(canReadActivity\) tabs\.push\(\{ value: "activity", label: "Activity" \}\)/);
  assert.match(detail, /<McpActivitySection appId=\{server\.app_id\} appFamilyId=\{server\.app_family_id\}/);
  assert.match(activity, /Navigate to=\{`\/integrations\/mcp\/\$\{id\}\?tab=activity`\}/);
  assert.match(activity, /searchParams\.get\("activity"\)/);
  assert.match(layout, /location\.pathname\.startsWith\("\/integrations\/mcp\/"\)/);
});

// Combined App details reuse the shared chrome and reveal MCP routes only for hosted delivery.
test("SDK and MCP details share app identity, navigation, overview, activity, and Changes components", () => {
  for (const route of [detail, sdkDetail]) {
    assert.match(route, /<AppDetailHeader/);
    assert.match(route, /<AppDetailBackLink/);
    assert.match(route, /<AppDetailPrimaryAction/);
    assert.match(route, /<AppVersionSwitcher/);
    assert.match(route, /<AppDetailTabs/);
    assert.match(route, /<AppDetailBody>/);
    assert.match(route, /<AppOverviewBody/);
    assert.match(route, /<AppChangesBody/);
  }
  assert.match(detailChrome, /export function AppDetailHeader/);
  assert.match(detailChrome, /export function AppDetailBackLink/);
  assert.match(detailChrome, /export function AppDetailPrimaryAction/);
  assert.match(detailChrome, /export function AppVersionSwitcher/);
  assert.match(detailChrome, /export function AppDetailTabs/);
  assert.match(detailBody, /export function AppDetailBody/);
  assert.match(detailBody, /export function AppOverviewBody/);
  assert.match(detailBody, /export function AppActivityBody/);
  assert.match(detailBody, /export function AppChangesBody/);
  assert.match(activity, /<AppActivityBody/);
  assert.match(detail, /<McpTransportEndpoints/);
  // Combined Apps expose MCP endpoints within the SDK-kind detail view only when hosted MCP is enabled.
  assert.match(sdkDetail, /optionalNode\(sdk\.hosted_mcp === true,[\s\S]*<McpTransportEndpoints/);
  assert.match(sdkDetail, /Download package/);
  assert.doesNotMatch(detail, /Download package|ReactMarkdown|LanguageBadge/);
});
