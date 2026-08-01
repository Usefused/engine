import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  hasAnyPermission,
  hasWorkspacePermission,
} from "./current-actor-permissions.ts";

const access = {
  subject_id: "user-1",
  workspace_id: "workspace-1",
  kind: "user",
  authorization_revision: 8,
  grants: [
    { permission: "workspace.read", resource_type: "WORKSPACE", resource_id: "workspace-1" },
    { permission: "service.read", resource_type: "SERVICE", resource_id: "service-1" },
  ],
};

test("workspace permission checks do not mistake a resource grant for workspace authority", () => {
  assert.equal(hasWorkspacePermission(access, "workspace.read"), true);
  assert.equal(hasWorkspacePermission(access, "service.read"), false);
  assert.equal(hasAnyPermission(access, "service.read"), true);
  assert.equal(hasWorkspacePermission(null, "workspace.read"), false);
});

test("uses the uppercase GraphQL ResourceType wire contract", () => {
  const lowercase = {
    ...access,
    grants: [{ permission: "workspace.read", resource_type: "workspace", resource_id: "workspace-1" }],
  };
  assert.equal(hasWorkspacePermission(lowercase, "workspace.read"), false);
});

test("access routes gate reads before mounting data loaders and gate management controls separately", () => {
  const people = source("../routes/integrations.access.people.tsx");
  const teams = source("../routes/integrations.access.teams.tsx");
  for (const route of [people, teams]) {
    assert.match(route, /WorkspacePermissionGate permission="access\.read"/);
    assert.match(route, /hasWorkspacePermission\(access, "access\.manage"\)/);
  }
  assert.match(people, /canManage && <AddPersonForm/);
  assert.match(people, /if \(status === "INVITED"\) return "Sign-in key required"/);
	assert.match(people, /hasWorkspacePermission\(access, "account\.manage"\)/);
	assert.match(people, /setSelectedOwnerProtected\(user\.owner_protected\)/);
	assert.match(people, /canManageOwners \|\| !selectedOwnerProtected/);
	assert.match(people, /Only a workspace Owner can change this person/);
  assert.match(teams, /if \(!props\.canManage\) return/);
  assert.match(teams, /hasWorkspacePermission\(access, "account\.manage"\)/);
  assert.match(teams, /const canEditTeam = props\.canManage && \(props\.canManageOwners \|\| teamWorkspaceRole/);
  assert.match(teams, /const disabled = !canEditTeam \|\| teamEditorDisabled/);
});

test("restricted access navigation is permission-aware and direct denial is explicit", () => {
  const sidebar = source("../components/layout/IntegrationsSidebar.tsx");
  const gate = source("../components/access/CurrentActorAccess.tsx");
  assert.match(sidebar, /People.*access\.read/);
  assert.match(sidebar, /Teams.*access\.read/);
  assert.match(gate, /Access not available/);
  assert.match(gate, /Ask a workspace administrator/);
  assert.doesNotMatch(gate, /No teams yet/);
});

function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}
