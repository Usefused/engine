import type { NotificationServiceRef, WorkspaceNotification } from "~/lib/api";

// notificationHelpers centralizes the matching/formatting rules Phase 4
// needs in more than one place (bell panel, contextual banners) so they
// can't drift apart -- see plans/plan-service-changelog.md's "## Phase 4".

// matchesService is the service-details-page filter: direct equality,
// service_id is never a joined/composite value.
export function matchesService(item: WorkspaceNotification, serviceId: string): boolean {
  return item.service_id === serviceId;
}

// matchesConfig is the SDK/MCP-details-page filter. This mirrors the
// backend's own sdkNotificationMatches (sdk_config_handlers.go), which is
// what SDKConfigPlanHandler's already-shipped notification filtering
// actually uses -- NOT a naive equality/split on config_key. An earlier
// draft of the Phase 4 plan doc assumed config_key (a comma-joined list of
// every impacted config) needed string-splitting; the real, already-proven
// rule is simpler: config_key exact-equality is only a fast path, the real
// check is service_id + version.
export function matchesConfig(
  item: WorkspaceNotification,
  configKey: string,
  serviceId: string,
  serviceVersion: string
): boolean {
  if (item.config_key === configKey) return true;
  if (item.service_id !== serviceId) return false;
  return !item.version || item.version === serviceVersion;
}

// isUnresolved excludes 'dismissed' -- the bell panel and banners both only
// ever show pending+acknowledged items, matching CreateWorkspaceNotification's
// own dedupe (which only ever matches pending rows) and the two-tier
// read/dismiss model's own semantics (dismissed = gone, not just quieter).
export function isUnresolved(item: WorkspaceNotification): boolean {
  return item.status !== "dismissed";
}

export function isPending(item: WorkspaceNotification): boolean {
  return item.status === "pending";
}

// worstSeverity picks the banner color: red if anything breaking is
// present, amber if only non-breaking items are, null if the list is empty
// (caller should render nothing in that case).
export function worstSeverity(items: WorkspaceNotification[]): "breaking" | "non-breaking" | null {
  if (items.length === 0) return null;
  return items.some((item) => item.severity === "breaking") ? "breaking" : "non-breaking";
}

// notificationTarget mirrors the CLI's own notificationTarget
// (cli/cmd/config_runner.go) so the UI and CLI describe the same
// notification the same way: prefer the specific config(s) it impacts,
// fall back to service+version, fall back to the raw type.
export function notificationTarget(item: WorkspaceNotification): string {
  if (item.config_key) return item.config_key;
  if (item.version && item.service_id) return `${item.service_id}@${item.version}`;
  if (item.service_id) return item.service_id;
  return item.type;
}

// notificationTypeLabel humanizes the raw type string ("registry_version_
// deprecated" -> "Version deprecated") for display -- the raw snake_case
// value is still shown elsewhere (title attribute, full page) for anyone
// who wants the exact type, this is just the friendly label.
const TYPE_LABELS: Record<string, string> = {
  registry_version_added: "New version published",
  registry_version_changed: "Version changed",
  registry_version_deprecated: "Version deprecated",
  registry_version_removed: "Version removed",
  registry_execution_policy_changed: "Execution policy changed",
  registry_connection_profile_changed: "Connection profile changed",
  workspace_service_removed: "Service force-removed",
  workspace_version_removed: "Service version force-removed",
};

export function notificationTypeLabel(item: WorkspaceNotification): string {
  return TYPE_LABELS[item.type] ?? item.type.replace(/_/g, " ");
}

// notificationTitle is the human-readable headline a row should lead with --
// the resolved service name (+ version, if this notification is scoped to
// one) when we have it, falling back through the same target hierarchy
// notificationTarget uses when no service name was resolved (e.g. the
// servicesByIds lookup hasn't finished yet, or failed). Never a bare UUID
// as the primary thing a person reads.
export function notificationTitle(
  item: WorkspaceNotification,
  serviceRefs: Record<string, NotificationServiceRef>
): string {
  const ref = item.service_id ? serviceRefs[item.service_id] : undefined;
  if (ref) return item.version ? (ref.name ? `${ref.name} — ${item.version}` : item.version) : (ref.name || notificationTypeLabel(item));
  if (item.config_key) return item.config_key;
  if (item.service_id) return item.version ? `${item.service_id}@${item.version}` : item.service_id;
  if (item.version) return item.version;
  return notificationTypeLabel(item);
}

// notificationLink is where a row should navigate on click -- the service's
// own details page (which resolves either a slug or a raw UUID, redirecting
// UUIDs to the canonical slug URL -- see integrations.$id.tsx's loader), or
// null when there's no service_id to link to at all.
export function notificationLink(
  item: WorkspaceNotification,
  serviceRefs: Record<string, NotificationServiceRef>
): string | null {
  if (!item.service_id) return null;
  const ref = serviceRefs[item.service_id];
  return `/integrations/${ref?.slug || item.service_id}`;
}
