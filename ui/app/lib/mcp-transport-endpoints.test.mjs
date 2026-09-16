import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

const componentPath = fileURLToPath(import.meta.resolve("../components/mcp/McpTransportEndpoints.tsx"));
const detailsPath = fileURLToPath(import.meta.resolve("../routes/integrations.mcp_.$id.tsx"));
const builderPath = fileURLToPath(import.meta.resolve("../routes/integrations.builder.tsx"));
const generationPanelPath = fileURLToPath(import.meta.resolve("../components/consumer/ConsumerGenerationPanel.tsx"));

test("keeps stable endpoints on Overview and pinned endpoints in version History", async () => {
  const source = await readFile(componentPath, "utf8");
  const overviewStart = source.indexOf("export function McpTransportEndpoints");
  const historyStart = source.indexOf("export function McpVersionTransportEndpoints");
  const overview = source.slice(overviewStart, historyStart);
  const history = source.slice(historyStart);

  assert.notEqual(overviewStart, -1);
  assert.notEqual(historyStart, -1);
  assert.match(overview, /<RecommendedEndpoint/);
  assert.match(overview, /<LegacyStableEndpoint/);
  assert.doesNotMatch(overview, /<VersionEndpoint/);
  assert.match(source, /SSE · Stable/);
  assert.match(source, /<TransportBadge>Stable<\/TransportBadge>/);
  assert.match(history, /Streamable HTTP · Version-pinned/);
  assert.match(history, /SSE · Version-pinned/);
  assert.match(history, /transport="versioned_streamable_http"/);
  assert.match(history, /<VersionEndpoint[\s\S]+legacy/);
  assert.doesNotMatch(history, /<TransportBadge>Pinned<\/TransportBadge>/);
});

test("uses Engine transport discovery in MCP details and deployment results", async () => {
  const [details, builder, generationPanel] = await Promise.all([
    readFile(detailsPath, "utf8"),
    readFile(builderPath, "utf8"),
    readFile(generationPanelPath, "utf8"),
  ]);

  assert.match(details, /default_transport/);
  assert.match(details, /stable_version_id/);
  assert.match(details, /transport_urls\s*\{\s*streamable_http\s+sse\s+versioned_streamable_http\s+versioned_sse\s*\}/);
  assert.match(details, /<McpTransportEndpoints endpoints=\{server\}/);
  assert.match(details, /<McpVersionTransportEndpoints/);
  assert.match(builder, /default_transport:\s*result\.default_transport/);
  assert.match(builder, /stable:\s*result\.stable/);
  assert.match(builder, /stable_version_id:\s*result\.stable_version_id/);
  assert.match(builder, /transport_urls:\s*result\.transport_urls/);
  assert.match(generationPanel, /<McpTransportEndpoints endpoints=\{mcpDeployment\}/);

  const combined = `${details}\n${builder}\n${generationPanel}`;
  assert.doesNotMatch(combined, /mcp_url/);
  assert.doesNotMatch(combined, /window\.location\.origin[^\n]*\/mcp\//);
});
