import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

// Reads colocated UI source so lifecycle placement remains covered without a browser-only assertion.
function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

const history = source("../components/apps/AppVersionHistory.tsx");
const sdkDetails = source("../routes/integrations.sdks.$id.tsx");
const mcpDetails = source("../routes/integrations.mcp_.$id.tsx");

test("SDK and REST Changes use the shared exact-version delete control", () => {
  assert.match(sdkDetails, /tabs\.push\(\{ value: "changes", label: "Changes" \}\)/);
  assert.match(sdkDetails, /<AppChangesBody/);
  assert.match(sdkDetails, /sdk\.delivery_mode === "api" \? "REST API" : "SDK"/);
  assert.match(sdkDetails, /api\.sdks\.deactivate\(version\.id\)/);
  assert.ok(sdkDetails.includes('hasResourcePermission(access, `app.${sdk.delivery_mode === "api" ? "api" : "sdk"}.manage`, "APP", sdk.app_family_id)'));
  assert.match(sdkDetails, /remaining\[0\]\.id\}\?tab=changes/);
});

test("MCP details expose Changes and delete one immutable family version", () => {
  assert.match(mcpDetails, /type McpDetailTab = "overview" \| "activity" \| "changes"/);
  assert.match(mcpDetails, /tabs\.push\(\{ value: "changes", label: "Changes" \}\)/);
  assert.match(mcpDetails, /<AppDetailTabs/);
  assert.match(mcpDetails, /<AppChangesBody/);
  assert.match(mcpDetails, /renderDetails=\{\(version\) => \(/);
  assert.match(mcpDetails, /version\.transport_urls/);
  assert.match(mcpDetails, /onSelect=\{\(appId\) => onNavigate\(`\/integrations\/mcp\/\$\{appId\}`\)\}/);
  assert.doesNotMatch(mcpDetails, /onSelect=\{\(appId\) => onNavigate\(`\/integrations\/mcp\/\$\{appId\}\?tab=changes`\)\}/);
  assert.match(mcpDetails, /api\.sdks\.deactivate\(version\.id\)/);
  assert.match(mcpDetails, /remaining\[0\]\.id\}\?tab=changes/);
});

test("version history hides destructive controls without family management", () => {
  assert.match(history, /\{canDelete \? \(/);
  assert.match(history, /disabled=\{Boolean\(deletingVersionId\)\}/);
  assert.match(history, /aria-label=\{`Delete version \$\{version\.version\}`\}/);
  assert.match(history, /version\.id === currentId/);
});
