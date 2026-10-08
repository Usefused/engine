import assert from "node:assert/strict";
import test from "node:test";
import { filterMcpItems, mcpItemKey, mcpCatalogPath } from "./mcp-catalog.ts";

// Search supports prompt arguments' surrounding metadata and resource URI discovery without schema noise.
test("catalog search matches metadata and URIs case insensitively", () => {
  const items = [{ name: "guide", description: "Team handbook", uri: "docs://handbook" }, { name: "search", inputSchema: { properties: { handbook: {} } } }];
  assert.deepEqual(filterMcpItems(items, " HANDBOOK "), [items[0]]);
  assert.equal(filterMcpItems(items, "docs://").length, 1);
  assert.equal(filterMcpItems(items, "").length, 2);
});

// Resources may share a display name while referring to different immutable upstream identities.
test("resource and template identities use their protocol URI", () => {
  assert.notEqual(mcpItemKey("resources", { name: "same", uri: "docs://a" }), mcpItemKey("resources", { name: "same", uri: "docs://b" }));
  assert.equal(mcpItemKey("resource_templates", { name: "issue", uriTemplate: "issues://{id}" }), "issues://{id}");
  assert.equal(mcpItemKey("tools", { name: "Search" }), "Search");
});

// Exact version IDs are encoded independently so route input cannot introduce another path segment.
test("catalog routes retain exact service and version boundaries", () => {
  assert.equal(mcpCatalogPath("service/one", "version?two"), "/workspace/services/service%2Fone/versions/version%3Ftwo/mcp-catalog");
});
