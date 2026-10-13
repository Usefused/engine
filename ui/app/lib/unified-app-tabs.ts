export type UnifiedAppTab = "overview" | "webhooks" | "schedules" | "analytics" | "requests" | "changes";

/** Places automation beside Overview without granting access to private triggers or execution history. */
export function unifiedAppTabs(canManage: boolean, canReadActivity: boolean): Array<{ value: UnifiedAppTab; label: string }> {
  const tabs: Array<{ value: UnifiedAppTab; label: string }> = [{ value: "overview", label: "Overview" }];
  // Trigger configuration includes private source settings and saved inputs, so both tabs require management.
  if (canManage) tabs.push({ value: "webhooks", label: "Webhooks" }, { value: "schedules", label: "Schedules" });
  // Audit access remains independent of app management.
  if (canReadActivity) tabs.push({ value: "analytics", label: "Analytics" }, { value: "requests", label: "Requests" });
  tabs.push({ value: "changes", label: "Versions" });
  return tabs;
}

/** Denied and unknown deep links never mount a protected tab's data loader. */
export function selectedUnifiedAppTab(tabs: Array<{ value: UnifiedAppTab }>, requested: string | null): UnifiedAppTab {
  return tabs.find((tab) => tab.value === requested)?.value ?? "overview";
}
