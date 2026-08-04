import assert from "node:assert/strict";
import test from "node:test";
import { renderToStaticMarkup } from "react-dom/server";

import {
  TeamAccessControls,
  changeWorkspaceRole,
  teamResourceLevel,
  teamArtifactAccessLevels,
  teamWorkspaceRole,
} from "../components/access/TeamAccessControls.ts";

const team = {
  id: "team-1",
  name: "Payments",
  slug: "payments",
  description: "Money movement",
  status: "active",
  created_at: "now",
  updated_at: "now",
  bindings: [
    binding("builder", "workspace", "workspace-1"),
    binding("service-user", "service", "service-1"),
    binding("bucket-manager", "bucket", "bucket-1"),
    { ...binding("artifact-reader", "artifact", "artifact-1"), resource_display_name: "Support SDK" },
  ],
};

test("renders simple workspace, service, and credential access controls", () => {
  const html = renderToStaticMarkup(TeamAccessControls({
    team,
    services: [{ id: "service-1", name: "Stripe" }],
    buckets: [{ id: "bucket-1", name: "Payments production" }],
    onWorkspaceRoleChange() {},
    onResourceAccessChange() {},
    onArtifactAccessChange() {},
  }));

  for (const label of ["Workspace role", "Builder", "Service access", "Stripe", "Credential access", "Payments production", "App and MCP server access", "Support SDK", "Read", "Share", "No access", "Use", "Manage"]) {
    assert.match(html, new RegExp(label));
  }
  assert.doesNotMatch(html, /webhook/i);
  assert.equal(teamWorkspaceRole(team), "BUILDER");
  assert.equal(teamResourceLevel(team, "service", "service-1"), "USER");
  assert.equal(teamResourceLevel(team, "bucket", "bucket-1"), "MANAGER");
  assert.deepEqual(teamArtifactAccessLevels(team, "artifact-1"), ["READER"]);
});

test("does not imply Viewer for a new team and allows Viewer to be selected", () => {
  const newTeam = { ...team, bindings: [] };
  assert.equal(teamWorkspaceRole(newTeam), "NONE");
  const html = renderToStaticMarkup(TeamAccessControls({
    team: newTeam,
    services: [],
    buckets: [],
    onWorkspaceRoleChange() {},
    onResourceAccessChange() {},
    onArtifactAccessChange() {},
  }));
  assert.match(html, /No workspace role/);

  let selected = "";
  changeWorkspaceRole("VIEWER", (role) => { selected = role; });
  assert.equal(selected, "VIEWER");
  changeWorkspaceRole("NONE", (role) => { selected = role; });
  assert.equal(selected, null);
});

test("keeps Owner role changes out of the Admin control surface", () => {
  const adminHTML = renderToStaticMarkup(TeamAccessControls({
    team,
    services: [],
    buckets: [],
    canManageOwners: false,
    onWorkspaceRoleChange() {},
    onResourceAccessChange() {},
    onArtifactAccessChange() {},
  }));
  assert.doesNotMatch(adminHTML, />Owner<\/option>/);
  assert.match(adminHTML, />Admin<\/option>/);

  const ownerTeam = { ...team, bindings: [binding("owner", "workspace", "workspace-1")] };
  const protectedHTML = renderToStaticMarkup(TeamAccessControls({
    team: ownerTeam,
    services: [],
    buckets: [],
    canManageOwners: false,
    onWorkspaceRoleChange() {},
    onResourceAccessChange() {},
    onArtifactAccessChange() {},
  }));
  assert.match(protectedHTML, /<select disabled=""[^>]*aria-label="Workspace role"/);
  assert.match(protectedHTML, />Owner<\/option>/);
});

test("models the same build as independently shareable with two teams", () => {
  const secondTeam = {
    ...team,
    id: "team-2",
    name: "Support",
    bindings: [{ ...binding("artifact-manager", "artifact", "artifact-1"), team_id: "team-2", resource_display_name: "Support SDK" }],
  };

  assert.deepEqual(teamArtifactAccessLevels(team, "artifact-1"), ["READER"]);
  assert.deepEqual(teamArtifactAccessLevels(secondTeam, "artifact-1"), ["MANAGER"]);
  const html = renderToStaticMarkup(TeamAccessControls({
    team: secondTeam,
    services: [],
    buckets: [],
    onWorkspaceRoleChange() {},
    onResourceAccessChange() {},
    onArtifactAccessChange() {},
  }));
  assert.match(html, /Support SDK/);
  assert.match(html, /value="MANAGER" selected=""/);
});

function binding(role, resourceType, resourceId) {
  return {
    id: `${role}-binding`,
    team_id: "team-1",
    role_slug: role,
    role_display_name: role,
    resource_type: resourceType,
    resource_id: resourceId,
    resource_display_name: "",
    created_at: "now",
  };
}
