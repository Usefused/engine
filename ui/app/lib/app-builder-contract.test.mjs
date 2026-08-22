import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import {
  APP_BUILDER_OPERATIONS,
  appApplyInput,
  appConfigKey,
  appPlanInput,
  effectiveAppBuilderServiceURL,
} from "./app-builder-contract.ts";

const appBuilderPath = fileURLToPath(import.meta.resolve("../routes/integrations.builder.tsx"));

function sourceSection(source, start, end) {
  const startIndex = source.indexOf(start);
  const endIndex = source.indexOf(end, startIndex + start.length);
  assert.notEqual(startIndex, -1, `missing section start: ${start}`);
  assert.notEqual(endIndex, -1, `missing section end: ${end}`);
  return source.slice(startIndex, endIndex);
}

test("uses exact owner-team and authorized selector GraphQL contracts", () => {
  assert.match(APP_BUILDER_OPERATIONS.owningTeams, /appOwningTeams\(search: \$search, limit: \$limit, offset: \$offset\)/);
  assert.match(APP_BUILDER_OPERATIONS.owningTeams, /items \{ id name slug \}/);
	assert.match(APP_BUILDER_OPERATIONS.selectors, /\$ownerTeamId: ID/);
  assert.match(APP_BUILDER_OPERATIONS.selectors, /\$resourceType: AppSelectorResourceType!/);
  assert.match(APP_BUILDER_OPERATIONS.selectors, /appBuildSelectors\(owner_team_id: \$ownerTeamId, resource_type: \$resourceType, search: \$search, limit: \$limit, offset: \$offset\)/);
  for (const field of ["resource_type", "resource_id", "display_name"]) {
    assert.match(APP_BUILDER_OPERATIONS.selectors, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(APP_BUILDER_OPERATIONS.selectors, /actor_allowed|team_allowed|missing/);
});

test("derives plan identity without embedding ownership in declarative config", () => {
  const config = { name: "support", version: "1.2.0", owner_team_id: "must-not-be-read" };
  assert.equal(appConfigKey("sdk", config), "sdk:support:1.2.0");
  assert.equal(appConfigKey("mcp", config), "mcp:support:1.2.0");
});

test("places ownership only on plan intent and makes apply owner-proof", () => {
  const config = { name: "support", version: "1.2.0", services: {} };
	const planned = appPlanInput("sdk", " support ", "sha256:abc", config);
	assert.deepEqual(planned, {
		owner_team: "support",
    config_key: "sdk:support:1.2.0",
    source_hash: "sha256:abc",
    config,
  });

	const applied = appApplyInput({
		plan_id: "plan-1",
		owner_type: "team",
    config_key: "sdk:support:1.2.0",
    source_hash: "sha256:abc",
    summary: {},
  });
  assert.deepEqual(applied, { plan_id: "plan-1", source_hash: "sha256:abc" });
	assert.equal("owner_team" in applied, false);

	assert.deepEqual(appPlanInput("sdk", "", "sha256:abc", config), {
		config_key: "sdk:support:1.2.0",
		source_hash: "sha256:abc",
		config,
	});
});

test("resolves the executable URL from an imported service contract", () => {
  assert.equal(effectiveAppBuilderServiceURL({ base_url: " https://api.example.test/v2 " }), "https://api.example.test/v2");
  assert.equal(effectiveAppBuilderServiceURL({
    base_url: "",
    servers: [
      { url: "https://sandbox.example.test" },
      { url: "https://api.example.test", is_default: true },
    ],
  }), "https://api.example.test");
  assert.equal(effectiveAppBuilderServiceURL({ base_url: null, servers: [] }), "");
});

test("hydrates the effective URL for catalog, expansion, and version changes", async () => {
  const source = await readFile(appBuilderPath, "utf8");
  const sections = [
    sourceSection(source, "async function loadRegistryServicesByIDs", "// AddSelectedServiceToWorkspaceButton"),
    sourceSection(source, "async function loadBuilderVersionContract", "// appServicesConfig"),
    sourceSection(source, "const webhookRes = await api.graphql", "const bootstrap = builderBootstrapRows"),
  ];

  for (const section of sections) {
    assert.match(section, /\bbase_url\b/);
    assert.match(section, /servers\s*\{\s*url\b/);
  }
  assert.match(source, /effectiveAppBuilderServiceURL\(serviceData\.service\)/);
});
