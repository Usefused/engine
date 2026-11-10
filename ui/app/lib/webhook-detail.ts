import type { CurrentActorAccess } from "./current-actor-permissions.ts";
import { hasResourcePermission, hasWorkspacePermission } from "./current-actor-permissions.ts";

export type WebhookDetailTab = "overview" | "analytics" | "events";
export type WebhookEventStatus = "" | "success" | "failed" | "rejected";

/** Matches app Activity: exact service visibility and workspace audit access are required before reporting mounts. */
export function canReadWebhookActivity(access: CurrentActorAccess | null, serviceID: string): boolean {
  return hasResourcePermission(access, "service.read", "SERVICE", serviceID) && hasWorkspacePermission(access, "audit.read");
}

/** Matches the shared app details hierarchy while withholding history from ordinary configuration readers. */
export function webhookDetailTabs(canReadActivity: boolean): Array<{ value: WebhookDetailTab; label: string }> {
  const tabs: Array<{ value: WebhookDetailTab; label: string }> = [{ value: "overview", label: "Overview" }];
  // Audit access is independent of the ability to view receiving URLs or manage signing secrets.
  if (canReadActivity) tabs.push({ value: "analytics", label: "Analytics" }, { value: "events", label: "Events" });
  return tabs;
}

/** Denied and unknown deep links resolve to a permitted view without issuing Activity queries. */
export function selectedWebhookTab(requested: string | null, canReadActivity: boolean): WebhookDetailTab {
  // Only explicit, authorized Activity selections may mount protected data.
  if (canReadActivity && (requested === "analytics" || requested === "events")) return requested;
  return "overview";
}

/** Keeps real event names selectable while letting the shared receipt filter own its reserved UNKNOWN option. */
export function webhookEventNameOptions(eventNames: string[], selected: string): string[] {
  // Rejected requests often have no trusted event type; do not duplicate their shared fallback option.
  return Array.from(new Set([selected, ...eventNames])).filter((name) => Boolean(name) && name !== "UNKNOWN");
}

/** Shared date conversion keeps summaries and history on the same inclusive UTC calendar bounds. */
export function webhookDateBound(value: string, end = false): string | null {
  // An omitted date leaves the historical range unrestricted instead of substituting today's date.
  if (!value) return null;
  // A date input is still untrusted at the API boundary; reject malformed values before constructing a timestamp.
  if (!/^\d{4}-\d{2}-\d{2}$/.test(value)) throw new Error("Choose a valid date.");
  // End dates include the complete selected day, matching the service Activity convention.
  return `${value}T${end ? "23:59:59.999" : "00:00:00.000"}Z`;
}
