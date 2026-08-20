// mutableWorkspaceNotificationID unwraps stored Engine IDs and rejects live Registry snapshots.
export function mutableWorkspaceNotificationID(id: string): string {
  // Registry drift rows have no Engine notification row, so sending their
  // display ID to the mutation could only produce a misleading failed write.
  if (id.startsWith("registry:")) {
    throw new Error("Registry drift snapshots cannot be updated");
  }
  return id.replace(/^engine:/, "");
}
