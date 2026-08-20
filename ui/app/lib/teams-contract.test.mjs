import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { TEAM_OPERATIONS } from "./teams-contract.ts";

const teamsPath = fileURLToPath(import.meta.resolve("./teams.ts"));

test("uses the Engine team read contract", () => {
  assert.match(TEAM_OPERATIONS.list, /teams\(search: \$search, limit: \$limit, offset: \$offset, include_archived: \$includeArchived\)/);
  assert.match(TEAM_OPERATIONS.editor, /team\(id: \$id\)/);
  assert.doesNotMatch(TEAM_OPERATIONS.editor, /workspaceServicePage|bucketSummaryPage/);
  assert.match(TEAM_OPERATIONS.services, /workspaceServicePage\(limit: \$limit, offset: \$offset\)\s*\{\s*total\s*data \{ service_id service_name \}/);
  assert.match(TEAM_OPERATIONS.buckets, /bucketSummaryPage\(limit: \$limit, offset: \$offset\)\s*\{\s*total\s*items \{ id name \}/);
  for (const field of ["id", "name", "slug", "description", "status", "bindings", "resource_display_name"]) {
    assert.match(TEAM_OPERATIONS.list, new RegExp(`\\b${field}\\b`));
  }
});

test("uses exact team metadata mutation inputs and payloads", () => {
  assert.match(TEAM_OPERATIONS.create, /\$input: CreateTeamInput!/);
  assert.match(TEAM_OPERATIONS.create, /createTeam\(input: \$input\)/);
  assert.match(TEAM_OPERATIONS.update, /\$input: UpdateTeamInput!/);
  assert.match(TEAM_OPERATIONS.update, /updateTeam\(id: \$id, input: \$input\)/);
  assert.match(TEAM_OPERATIONS.archive, /archiveTeam\(id: \$id\)/);
  for (const document of [TEAM_OPERATIONS.create, TEAM_OPERATIONS.update, TEAM_OPERATIONS.archive]) {
    assert.match(document, /authorization_revision changed/);
  }
});

test("uses the same exact binding fields and enum names as the CLI", () => {
  assert.match(TEAM_OPERATIONS.workspaceRole, /\$role: TeamWorkspaceRole\)/);
  assert.doesNotMatch(TEAM_OPERATIONS.workspaceRole, /TeamWorkspaceRole!/);
  assert.match(TEAM_OPERATIONS.workspaceRole, /setTeamWorkspaceRole\(team_id: \$teamId, role: \$role\)/);
  const resourceOperations = [
    [TEAM_OPERATIONS.grantService, "grantTeamServiceAccess", "service_id"],
    [TEAM_OPERATIONS.revokeService, "revokeTeamServiceAccess", "service_id"],
    [TEAM_OPERATIONS.grantBucket, "grantTeamBucketAccess", "bucket_id"],
    [TEAM_OPERATIONS.revokeBucket, "revokeTeamBucketAccess", "bucket_id"],
  ];
  for (const [document, field, resourceArgument] of resourceOperations) {
    assert.match(document, /\$level: TeamAccessLevel!/);
    assert.match(document, new RegExp(`${field}\\(team_id: \\$teamId, ${resourceArgument}: \\$resourceId, level: \\$level\\)`));
    assert.match(document, /binding \{ id team_id role_slug role_display_name resource_type resource_id resource_display_name created_at \}/);
  }

  const appOperations = [
    [TEAM_OPERATIONS.grantApp, "grantTeamAppAccess"],
    [TEAM_OPERATIONS.revokeApp, "revokeTeamAppAccess"],
  ];
  for (const [document, field] of appOperations) {
    assert.match(document, /\$level: TeamAppAccessLevel!/);
    assert.match(document, new RegExp(`${field}\\(team_id: \\$teamId, app_family_id: \\$resourceId, level: \\$level\\)`));
    assert.match(document, /authorization_revision changed/);
  }
});

test("loads independently authorized resource catalogues in bounded pages", async () => {
  const source = await readFile(teamsPath, "utf8");
  assert.match(source, /access\.services \? loadTeamServices\(\) : Promise\.resolve\(\[\]\)/);
  assert.match(source, /access\.buckets \? loadTeamBuckets\(\) : Promise\.resolve\(\[\]\)/);
  assert.match(source, /TEAM_RESOURCE_PAGE_SIZE = 100/);
  assert.match(source, /readAllBoundedPages/);
  assert.match(source, /TEAM_RESOURCE_PAGE_SIZE, TEAM_RESOURCE_MAX_PAGES/);
});
