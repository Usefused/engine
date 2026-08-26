import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

// source reads a UI module relative to this test without depending on the
// process working directory used by the test runner.
function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

// assertResponsivePair verifies that a narrow card list and wider table are
// both present so mobile support cannot silently regress to page scrolling.
function assertResponsivePair(contents, label) {
  assert.match(contents, /md:hidden/, `${label} must expose a mobile layout`);
  assert.match(contents, /hidden overflow-x-auto md:block/, `${label} must retain a desktop table`);
}

test("app Activity uses cards on mobile and tables on wider screens", () => {
  const requests = source("../components/activity/AppRequestsPanel.tsx");
  const overview = source("../components/activity/AppActivityOverview.tsx");

  assertResponsivePair(requests, "App receipts");
  assertResponsivePair(overview, "App service usage");
  assert.match(requests, /grid w-full grid-cols-1 gap-2 sm:flex sm:w-auto/, "Receipt filters must stack within the mobile card width");
  assert.match(requests, /break-all font-mono text-\[11px\]/, "Provider paths must wrap inside mobile receipts");
});

// Session history keeps its responsive pairing when moved out of the route for cursor pagination.
test("MCP Activity uses responsive cards for usage and sessions", () => {
  const analytics = source("../components/mcp/McpAnalyticsPanel.tsx");
  const route = source("../routes/integrations.mcp_.$id.analytics.tsx");
  const sessions = source("../components/mcp/McpSessionsPanel.tsx");

  assertResponsivePair(analytics, "MCP usage");
  assertResponsivePair(sessions, "MCP sessions");
  assert.match(analytics, /function McpUsageCards/, "Tool and service mobile layouts must share one card implementation");
  assert.match(sessions, /function McpSessionCard/, "Session history must expose its mobile card layout");
  assert.match(route, /function McpTokenActivityCard/, "Token history must expose its mobile card layout");
  assert.match(route, /tokenTermination\(token\)/, "Expired and revoked tokens must render retained termination evidence");
  assert.match(route, /min-w-0 max-w-full.*overflow-x-hidden/, "The MCP Activity shell must contain narrow-width content");
});

test("Activity tabs scroll locally without widening their page", () => {
  const tabs = source("../components/activity/NestedActivityTabs.tsx");
  const sdk = source("../routes/integrations.sdks.$id.tsx");

  assert.match(tabs, /max-w-full overflow-x-auto overscroll-x-contain/, "Nested tabs must own their horizontal overflow");
  assert.match(tabs, /\[scrollbar-width:none\]/, "Touch tabs must avoid a persistent mobile scrollbar");
  assert.match(sdk, /min-w-0 max-w-full space-y-5 overflow-x-hidden/, "App Activity must contain child overflow at the page boundary");
});
