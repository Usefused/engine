import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

const list = source("../routes/integrations.mcp.tsx");
const detail = source("../routes/integrations.mcp_.$id.tsx");
const activity = source("../routes/integrations.mcp_.$id.analytics.tsx");
const services = source("../components/apps/AppConnectedServices.tsx");
const layout = source("../routes/integrations.tsx");

test("MCP catalogue mirrors the searchable SDK list and rows open exact details", () => {
  assert.match(list, /query MCPApps\(\$search: String!, \$version: String!, \$limit: Int!, \$offset: Int!\)/);
  assert.match(list, /apps\(kind: "mcp", search: \$search, version: \$version/);
  assert.match(list, /onNavigate=\{\(id\) => navigate\(`\/integrations\/mcp\/\$\{id\}`\)\}/);
  assert.match(list, /<McpPagination page=\{page\} total=\{total\}/);
  assert.doesNotMatch(list, /<McpTransportEndpoints/);
});

test("MCP details show Engine-projected transports and immutable selected operations", () => {
  assert.match(detail, /app\(app_id: \$appId\)/);
  assert.match(detail, /appServices\(app_id: \$appId\)/);
  assert.match(detail, /operation_names/);
  assert.match(detail, /result\.app\.kind !== "mcp"/);
  assert.match(detail, /<McpTransportEndpoints endpoints=\{server\}/);
  assert.match(detail, /<AppConnectedServices selections=\{server\.detailed_selections\}/);
  assert.match(services, /appSelectionDisplayRows\(selection\.endpoint_ids, selection\.operation_names\)/);
  assert.doesNotMatch(services, /api\.|mcpGraphql|Registry/);
});

test("MCP Activity is a permission-gated detail tab and the old URL redirects", () => {
  assert.match(detail, /canReadActivity \? <button[^>]+[\s\S]+Activity/);
  assert.match(detail, /<McpActivitySection appId=\{server\.app_id\} appFamilyId=\{server\.app_family_id\}/);
  assert.match(activity, /Navigate to=\{`\/integrations\/mcp\/\$\{id\}\?tab=activity`\}/);
  assert.match(activity, /searchParams\.get\("activity"\)/);
  assert.match(layout, /location\.pathname\.startsWith\("\/integrations\/mcp\/"\)/);
});
