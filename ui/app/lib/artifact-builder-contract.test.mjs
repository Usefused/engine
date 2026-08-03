import assert from "node:assert/strict";
import test from "node:test";

import {
  ARTIFACT_BUILDER_OPERATIONS,
  artifactApplyInput,
  artifactConfigKey,
  artifactPlanInput,
} from "./artifact-builder-contract.ts";

test("uses exact owner-team and authorized selector GraphQL contracts", () => {
  assert.match(ARTIFACT_BUILDER_OPERATIONS.owningTeams, /artifactOwningTeams\(search: \$search, limit: \$limit, offset: \$offset\)/);
  assert.match(ARTIFACT_BUILDER_OPERATIONS.owningTeams, /items \{ id name slug \}/);
	assert.match(ARTIFACT_BUILDER_OPERATIONS.selectors, /\$ownerTeamId: ID/);
  assert.match(ARTIFACT_BUILDER_OPERATIONS.selectors, /\$resourceType: ArtifactSelectorResourceType!/);
  assert.match(ARTIFACT_BUILDER_OPERATIONS.selectors, /artifactBuildSelectors\(owner_team_id: \$ownerTeamId, resource_type: \$resourceType, search: \$search, limit: \$limit, offset: \$offset\)/);
  for (const field of ["resource_type", "resource_id", "display_name"]) {
    assert.match(ARTIFACT_BUILDER_OPERATIONS.selectors, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(ARTIFACT_BUILDER_OPERATIONS.selectors, /actor_allowed|team_allowed|missing/);
});

test("derives plan identity without embedding ownership in declarative config", () => {
  const config = { name: "support", version: "1.2.0", owner_team_id: "must-not-be-read" };
  assert.equal(artifactConfigKey("sdk", config), "sdk:support:1.2.0");
  assert.equal(artifactConfigKey("mcp", config), "mcp:support:1.2.0");
  assert.equal(artifactConfigKey("webhook", { name: "alerts" }), "webhook:alerts");
});

test("places ownership only on plan intent and makes apply owner-proof", () => {
  const config = { name: "support", version: "1.2.0", services: {} };
	const planned = artifactPlanInput("sdk", " support ", "sha256:abc", config);
	assert.deepEqual(planned, {
		owner_team: "support",
    config_key: "sdk:support:1.2.0",
    source_hash: "sha256:abc",
    config,
  });

	const applied = artifactApplyInput({
		plan_id: "plan-1",
		owner_type: "team",
    config_key: "sdk:support:1.2.0",
    source_hash: "sha256:abc",
    summary: {},
  });
  assert.deepEqual(applied, { plan_id: "plan-1", source_hash: "sha256:abc" });
	assert.equal("owner_team" in applied, false);

	assert.deepEqual(artifactPlanInput("sdk", "", "sha256:abc", config), {
		config_key: "sdk:support:1.2.0",
		source_hash: "sha256:abc",
		config,
	});
});
