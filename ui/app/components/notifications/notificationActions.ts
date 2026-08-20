// canMutateNotification allows actions only for durable Engine notification rows.
export function canMutateNotification(source: string, canUpdate: boolean): boolean {
  // Registry rows are live drift snapshots rather than stored notifications,
  // so there is no durable status for acknowledge or dismiss to update.
  return canUpdate && source === "engine";
}
