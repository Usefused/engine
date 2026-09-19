import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  hasAnyPermission,
  hasAnyAppPermission,
  hasResourcePermission,
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

test("resource permission checks honor exact bucket grants and workspace inheritance", () => {
  const bucketAccess = {
    ...access,
    grants: [
      { permission: "credentials.metadata.read", resource_type: "BUCKET", resource_id: "bucket-1" },
      { permission: "connection.manage", resource_type: "BUCKET", resource_id: "bucket-2" },
      { permission: "bucket.manage", resource_type: "WORKSPACE", resource_id: "workspace-1" },
    ],
  };
  assert.equal(hasResourcePermission(bucketAccess, "credentials.metadata.read", "BUCKET", "bucket-1"), true);
  assert.equal(hasResourcePermission(bucketAccess, "credentials.metadata.read", "BUCKET", "bucket-2"), false);
  assert.equal(hasResourcePermission(bucketAccess, "connection.manage", "BUCKET", "bucket-1"), false);
  assert.equal(hasResourcePermission(bucketAccess, "bucket.manage", "BUCKET", "bucket-9"), true);
  assert.equal(hasResourcePermission(bucketAccess, "bucket.manage", "SERVICE", "service-1"), true);
  assert.equal(hasResourcePermission(null, "bucket.manage", "BUCKET", "bucket-1"), false);
});

test("credential and lifecycle surfaces gate protected queries and actions", () => {
  const buckets = source("../routes/integrations.buckets.tsx");
  const bucketQueries = source("./buckets.ts");
  const apps = source("../routes/integrations.sdks._index.tsx");
  const profile = source("../components/integration-details/WorkspaceConnectionProfileSection.tsx");
  const notifications = source("../components/notifications/NotificationList.tsx");
  const notificationActions = source("../components/notifications/notificationActions.ts");
  const builder = source("../routes/integrations.builder.tsx");

  assert.match(buckets, /values: hasWorkspacePermission\(access, "bucket\.values\.read"\)/);
  assert.match(buckets, /credentials\.metadata\.read/);
  assert.match(buckets, /canCreateBucket = hasWorkspacePermission\(access, "bucket\.manage"\)/);
  assert.match(bucketQueries, /buckets: bucketSummaries/);
  assert.doesNotMatch(bucketQueries, /query \{\s*buckets \{/);
  assert.ok(apps.includes('hasResourcePermission(access, `app.${appTargetType(sdk)}.manage`, "APP", sdk.app_family_id)'));
  assert.match(profile, /if \(!canRead\)/);
  assert.match(profile, /\{canManage && \(/);
  assert.match(notifications, /canMutateNotification\(item\.source, canUpdate\) && <NotificationActions/);
  assert.match(notificationActions, /canUpdate && source === "engine"/);
  assert.match(builder, /if \(!canReadApps\)/);
  assert.match(builder, /isAuth && canReadServices && workspaceServicesLoaded/);
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
  const apps = source("../routes/integrations.sdks._index.tsx");
  const gate = source("../components/access/CurrentActorAccess.tsx");
  assert.match(sidebar, /label: "Access"/);
  assert.match(sidebar, /label: "Credentials"/);
  assert.doesNotMatch(sidebar, /label: "Create app"/);
  assert.match(apps, /<CreateAppMenu/);
  assert.doesNotMatch(apps, /createTo|createLabel/);
  assert.match(sidebar, /visible: \(access\) => hasAnyPermission\(access, "bucket\.read"\)/);
  assert.match(sidebar, /visible: \(access\) => hasWorkspacePermission\(access, "access\.read"\)/);
  assert.match(gate, /Access not available/);
  assert.match(gate, /Ask a workspace administrator/);
  assert.doesNotMatch(gate, /No teams yet/);
});

// Reads a colocated UI source file for permission-contract assertions.
function source(relativePath) {
  return readFileSync(new URL(relativePath, import.meta.url), "utf8");
}

// A permission for one app type must not enable a sibling type's creation controls.
test("app permissions keep creation and resource access type-specific", () => {
  const narrow = { ...access, grants: [
    { permission: "app.mcp.create", resource_type: "WORKSPACE", resource_id: "workspace-1" },
    { permission: "app.mcp.read", resource_type: "APP", resource_id: "mcp-1" },
  ] };
  assert.equal(hasWorkspacePermission(narrow, "app.mcp.create"), true);
  for (const type of ["sdk", "api", "webhook"]) {
    assert.equal(hasWorkspacePermission(narrow, `app.${type}.create`), false);
    assert.equal(hasResourcePermission(narrow, `app.${type}.read`, "APP", "mcp-1"), false);
  }
  assert.equal(hasAnyAppPermission(narrow, "read"), true);
  assert.equal(hasAnyAppPermission(narrow, "manage"), false);
  assert.equal(hasWorkspacePermission(narrow, "app.create"), false);
});
