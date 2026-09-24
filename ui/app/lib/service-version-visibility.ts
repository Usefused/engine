/** Distinguishes consumer visibility from the legacy "public" lifecycle status, which only means non-deprecated. */
export function versionStateLabel(version: { is_public: boolean; status?: string }): string {
  // Lifecycle warnings remain visible even when consumer access is enabled.
  if (version.status === "deprecated") return "Deprecated";
  // Retain compatibility with a draft lifecycle without treating private releases as drafts.
  if (version.status === "draft") return "Draft";
  // Consumer access is determined exclusively by the persisted visibility flag.
  return version.is_public ? "Public" : "Private";
}
