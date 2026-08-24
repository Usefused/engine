import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";
import {
  canReadWorkspaceActivityOverview,
  canReadWorkspaceNotifications,
  selectedWorkspaceActivityTab,
  workspaceActivityTabs,
} from "./activity-access.ts";

const activityRoute = readFileSync(new URL("../routes/integrations.activity.tsx", import.meta.url), "utf8");
const sidebar = readFileSync(new URL("../components/layout/IntegrationsSidebar.tsx", import.meta.url), "utf8");
const bell = readFileSync(new URL("../components/notifications/NotificationBell.tsx", import.meta.url), "utf8");
const notificationHook = readFileSync(new URL("../components/notifications/useWorkspaceNotifications.ts", import.meta.url), "utf8");
const paginatedHook = readFileSync(new URL("../components/notifications/usePaginatedWorkspaceNotifications.ts", import.meta.url), "utf8");
const serviceRoute = readFileSync(new URL("../routes/integrations.$id.tsx", import.meta.url), "utf8");
const sdkRoute = readFileSync(new URL("../routes/integrations.sdks.$id.tsx", import.meta.url), "utf8");
const mcpActivity = readFileSync(new URL("../routes/integrations.mcp_.$id.analytics.tsx", import.meta.url), "utf8");
const mcpDetail = readFileSync(new URL("../routes/integrations.mcp_.$id.tsx", import.meta.url), "utf8");

// workspaceAccess constructs one exact workspace-scoped authorization snapshot
// without granting any implied capability.
function workspaceAccess(...permissions) {
  return {
    subject_id: "subject",
    workspace_id: "workspace",
    kind: "user",
    authorization_revision: 1,
    grants: permissions.map((permission) => ({ permission, resource_type: "WORKSPACE", resource_id: "workspace" })),
  };
}

test("workspace Activity exposes only permission-backed tabs", () => {
  const neither = workspaceAccess();
  const auditor = workspaceAccess("audit.read");
  const notificationReader = workspaceAccess("workspace.read");
  const both = workspaceAccess("audit.read", "workspace.read");

  assert.deepEqual(workspaceActivityTabs(neither), []);
  assert.deepEqual(workspaceActivityTabs(auditor), ["overview"]);
  assert.deepEqual(workspaceActivityTabs(notificationReader), ["notifications"]);
  assert.deepEqual(workspaceActivityTabs(both), ["overview", "notifications"]);
  assert.equal(canReadWorkspaceActivityOverview(auditor), true);
  assert.equal(canReadWorkspaceNotifications(auditor), false);
  assert.equal(canReadWorkspaceNotifications(notificationReader), true);
});

test("unauthorized Activity URL selections fall back without mounting denied data", () => {
  assert.equal(selectedWorkspaceActivityTab("notifications", ["overview"]), "overview");
  assert.equal(selectedWorkspaceActivityTab(null, ["notifications"]), "notifications");
  assert.equal(selectedWorkspaceActivityTab("overview", []), null);
  assert.match(activityRoute, /const tabs = workspaceActivityTabs\(access\)/);
  assert.match(activityRoute, /tabs\.map\(\(value\)/);
  assert.match(activityRoute, /if \(!tab\) return <AccessDenied/);
});

test("notification discovery waits for read authorization and hides the bell", () => {
  for (const source of [notificationHook, paginatedHook]) {
    assert.match(source, /canReadWorkspaceNotifications\(access\)/);
    assert.match(source, /shouldLoad/);
    assert.match(source, /if \(!shouldLoad\) return Promise\.resolve\(\)/);
  }
  assert.match(bell, /if \(!canRead\) return null/);
  assert.match(sidebar, /workspaceActivityTabs\(access\)\.length > 0/);
});

test("scoped Activity tabs are hidden when their audit capability is absent", () => {
  assert.match(serviceRoute, /workspaceServiceActive === true && canReadActivity/);
  assert.match(serviceRoute, /return canReadActivity \? <ActivityTabContent \/> : <EndpointTabContent \/>/);
  assert.match(sdkRoute, /optionalNode\(canReadActivity, \(\s*<button[\s\S]+Activity/);
  assert.match(sdkRoute, /requestedActiveTab === "analytics" && !canReadActivity \? "overview"/);
  assert.match(mcpDetail, /hasResourcePermission\(access, "app\.read", "APP", state\.server\?\.app_family_id/);
  assert.match(mcpDetail, /requestedTab === "activity" && !canReadActivity \? "overview"/);
  assert.match(mcpDetail, /canReadActivity \? <button[^>]+[\s\S]+Activity/);
  assert.match(mcpActivity, /hasResourcePermission\(access, "app\.read", "APP", appFamilyId\) && hasWorkspacePermission\(access, "audit\.read"\)/);
  assert.doesNotMatch(mcpActivity, /hasAnyPermission\(access, "app\.read"\)/);
});
