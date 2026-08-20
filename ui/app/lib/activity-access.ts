import type { CurrentActorAccess } from "./current-actor-permissions.ts";
import { hasWorkspacePermission } from "./current-actor-permissions.ts";

export type WorkspaceActivityTab = "overview" | "notifications";

// canReadWorkspaceActivityOverview keeps the workspace analytics capability
// aligned with the Engine GraphQL audit boundary.
export function canReadWorkspaceActivityOverview(access: CurrentActorAccess | null): boolean {
  return hasWorkspacePermission(access, "audit.read");
}

// canReadWorkspaceNotifications keeps notification discovery aligned with the
// workspace-scoped read boundary; updates remain a separate capability.
export function canReadWorkspaceNotifications(access: CurrentActorAccess | null): boolean {
  return hasWorkspacePermission(access, "workspace.read");
}

// workspaceActivityTabs returns only views the current actor can actually
// query, preserving a stable order for the default selection.
export function workspaceActivityTabs(access: CurrentActorAccess | null): WorkspaceActivityTab[] {
  const tabs: WorkspaceActivityTab[] = [];
  if (canReadWorkspaceActivityOverview(access)) tabs.push("overview");
  if (canReadWorkspaceNotifications(access)) tabs.push("notifications");
  return tabs;
}

// selectedWorkspaceActivityTab rejects an unauthorized URL selection and
// falls back to the first permitted view without mounting the denied view.
export function selectedWorkspaceActivityTab(
  requested: string | null,
  tabs: WorkspaceActivityTab[]
): WorkspaceActivityTab | null {
  const normalized = requested === "notifications" ? "notifications" : "overview";
  return tabs.includes(normalized) ? normalized : tabs[0] ?? null;
}
