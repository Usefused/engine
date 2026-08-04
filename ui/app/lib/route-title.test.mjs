import assert from "node:assert/strict";
import test from "node:test";

import { routeTitle } from "./route-title.ts";

test("titles every primary Engine route", () => {
  const routes = new Map([
    ["/login", "Sign in - Fused"],
    ["/integrations", "Services - Fused"],
    ["/integrations/sdks", "Apps - Fused"],
    ["/integrations/mcp", "MCP servers - Fused"],
    ["/integrations/access/people", "People - Fused"],
    ["/integrations/access/teams", "Teams - Fused"],
    ["/integrations/buckets", "Credentials - Fused"],
	["/integrations/activity", "Activity - Fused"],
    ["/integrations/settings", "Settings - Fused"],
  ]);
  for (const [path, title] of routes) assert.equal(routeTitle(path), title);
});

test("titles route-specific creation and detail pages", () => {
  assert.equal(routeTitle("/integrations/builder"), "Create app - Fused");
  assert.equal(routeTitle("/integrations/builder", "?tab=mcp"), "Create MCP server - Fused");
  assert.equal(routeTitle("/integrations/sdks/app-id"), "App details - Fused");
  assert.equal(routeTitle("/integrations/mcp/server-id/analytics"), "MCP server activity - Fused");
  assert.equal(routeTitle("/integrations/stripe"), "Service details - Fused");
  assert.equal(routeTitle("/integrations/acme/stripe"), "Service details - Fused");
});
