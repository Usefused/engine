import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

const menuPath = fileURLToPath(import.meta.resolve("../components/apps/CreateAppMenu.tsx"));
const cataloguePath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks._index.tsx"));
const builderPath = fileURLToPath(import.meta.resolve("../components/apps/AppServiceBuilder.tsx"));
const generationPanelPath = fileURLToPath(import.meta.resolve("../components/consumer/ConsumerGenerationPanel.tsx"));

// The menu contract keeps only the four requested app types in their catalogue order.
test("offers every supported app delivery adapter from one accessible create menu", async () => {
  const menu = await readFile(menuPath, "utf8");

  assert.match(menu, /aria-haspopup="menu"/);
  assert.match(menu, /role="menu"/);
  assert.match(menu, /service \? "Use in App" : "Create App"/);
  assert.doesNotMatch(menu, />\s*Create app\s*</);
  assert.doesNotMatch(menu, /mode: "app", label: "App"/);
  assert.match(menu, /\/integrations\/unified-apps\/new/);
  assert.match(menu, /\/integrations\/builder\?tab=sdk/);
  assert.match(menu, /\/integrations\/builder\?tab=api/);
  assert.match(menu, /\/integrations\/builder\?tab=mcp/);
  assert.ok(menu.indexOf('mode: "mcp"') < menu.indexOf('mode: "unified_app"'));
  assert.ok(menu.indexOf('mode: "unified_app"') < menu.indexOf('mode: "sdk"'));
  assert.ok(menu.indexOf('mode: "sdk"') < menu.indexOf('mode: "api"'));
  assert.match(menu, /Generate a typed package/);
  assert.match(menu, /Call operations through Fused/);
  assert.match(menu, /Connect agents to selected operations/);
});

// Empty and populated catalogues must expose the same creation choices.
test("reuses the create menu in populated and empty app catalogue states", async () => {
  const catalogue = await readFile(cataloguePath, "utf8");
  const occurrences = catalogue.match(/<CreateAppMenu/g) ?? [];

  assert.equal(occurrences.length, 2);
  assert.doesNotMatch(catalogue, /<Link\s+to="\/integrations\/builder"/);
  // The Engine filters before pagination, so each tab's total matches its visible type.
  assert.match(catalogue, /appFamilies\(kind: \$kind, search: \$search/);
  assert.match(catalogue, /<AppCatalogueTypeTabs selected=\{type\} onSelect=\{selectType\}/);
  assert.match(catalogue, /type: "mcp"[\s\S]*type: "unified_app"[\s\S]*type: "sdk"[\s\S]*type: "api"/);
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
