import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

const componentPath = fileURLToPath(import.meta.resolve("../components/mcp/McpTransportEndpoints.tsx"));
const managementPath = fileURLToPath(import.meta.resolve("../routes/integrations.mcp.tsx"));
const builderPath = fileURLToPath(import.meta.resolve("../routes/integrations.builder.tsx"));
const generationPanelPath = fileURLToPath(import.meta.resolve("../components/consumer/ConsumerGenerationPanel.tsx"));

test("presents Streamable HTTP first and keeps legacy SSE collapsed", async () => {
  const source = await readFile(componentPath, "utf8");
  const primaryIndex = source.indexOf("Streamable HTTP");
  const legacyIndex = source.indexOf("<details");

  assert.notEqual(primaryIndex, -1);
  assert.notEqual(legacyIndex, -1);
  assert.ok(primaryIndex < legacyIndex, "Streamable HTTP must render before compatibility transports");
  assert.match(source, /<TransportBadge>Recommended<\/TransportBadge>/);
  assert.match(source, /<span>Legacy compatibility<\/span>/);
  assert.match(source, /<TransportBadge legacy>Legacy<\/TransportBadge>/);
  assert.doesNotMatch(source, /<details[^>]*\sopen(?:=|\s|>)/, "legacy compatibility must be collapsed by default");
});

test("uses Engine transport discovery in MCP management and deployment results", async () => {
  const [management, builder, generationPanel] = await Promise.all([
    readFile(managementPath, "utf8"),
    readFile(builderPath, "utf8"),
    readFile(generationPanelPath, "utf8"),
  ]);

  assert.match(management, /default_transport/);
  assert.match(management, /transport_urls\s*\{\s*streamable_http\s+sse\s*\}/);
  assert.match(management, /<McpTransportEndpoints endpoints=\{server\}/);
  assert.match(builder, /default_transport:\s*result\.default_transport/);
  assert.match(builder, /transport_urls:\s*result\.transport_urls/);
  assert.match(generationPanel, /<McpTransportEndpoints endpoints=\{mcpDeployment\}/);

  const combined = `${management}\n${builder}\n${generationPanel}`;
  assert.doesNotMatch(combined, /mcp_url/);
  assert.doesNotMatch(combined, /window\.location\.origin[^\n]*\/mcp\//);
});
