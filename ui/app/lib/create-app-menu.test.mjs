import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

const menuPath = fileURLToPath(import.meta.resolve("../components/apps/CreateAppMenu.tsx"));
const cataloguePath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks._index.tsx"));
const builderPath = fileURLToPath(import.meta.resolve("../routes/integrations.builder.tsx"));
const generationPanelPath = fileURLToPath(import.meta.resolve("../components/consumer/ConsumerGenerationPanel.tsx"));

// The menu contract keeps every delivery adapter reachable from the unified Apps catalogue.
test("offers every supported app delivery adapter from one accessible create menu", async () => {
  const menu = await readFile(menuPath, "utf8");

  assert.match(menu, /aria-haspopup="menu"/);
  assert.match(menu, /role="menu"/);
  assert.match(menu, /primaryOption\?\.mode === "app" \? "\/integrations\/builder"/);
  assert.match(menu, /\/integrations\/builder\?tab=app/);
  assert.match(menu, /\/integrations\/builder\?tab=sdk/);
  assert.match(menu, /\/integrations\/builder\?tab=api/);
  assert.match(menu, /\/integrations\/builder\?tab=mcp/);
  assert.match(menu, /Generate a typed package/);
  assert.match(menu, /Call operations through the Engine/);
  assert.match(menu, /Connect agents over MCP/);
});

// Empty and populated catalogues must expose the same creation choices.
test("reuses the create menu in populated and empty app catalogue states", async () => {
  const catalogue = await readFile(cataloguePath, "utf8");
  const occurrences = catalogue.match(/<CreateAppMenu/g) ?? [];

  assert.equal(occurrences.length, 2);
  assert.doesNotMatch(catalogue, /<Link\s+to="\/integrations\/builder"/);
});

// An untyped builder route starts the combined flow without an extra delivery-choice step.
test("defaults the builder to a combined App", async () => {
  const builder = await readFile(builderPath, "utf8");

  assert.match(builder, /appCreationModeFromSearch\(params\)/);
  assert.doesNotMatch(builder, /BuilderModeSelectionPage/);
});

// A single-company Engine names app families locally instead of using package-manager scopes.
test("uses company-local names for every app type", async () => {
  const panel = await readFile(generationPanelPath, "utf8");

  assert.match(panel, /"customer-sdk"/);
  assert.match(panel, /"customer-api"/);
  assert.match(panel, /"customer-support"/);
  assert.doesNotMatch(panel, /@myorg/);
  assert.match(panel, /"SDK name"/);
  assert.match(panel, /"API name"/);
  assert.match(panel, /"Server name"/);
});
