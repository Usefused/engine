export interface AppVersionIdentity { app_id: string }

interface DeactivationDependencies {
  listVersions: (familyId: string) => Promise<AppVersionIdentity[]>;
  confirm: (versionCount: number) => Promise<boolean>;
  deactivate: (appId: string) => Promise<unknown>;
}

export interface FamilyDeactivationResult {
  cancelled: boolean;
  removed: number;
  failed: number;
  remaining: number | null;
}

/** Removes only the reviewed version snapshot through the normal per-version cleanup path. */
export async function deactivateFamilyVersions(familyIds: string[], dependencies: DeactivationDependencies): Promise<FamilyDeactivationResult> {
  const families = [...new Set(familyIds)];
  const versions: AppVersionIdentity[] = [];
  // Read every selected family before confirmation so discovery failures cannot cause partial deletion.
  for (const familyId of families) versions.push(...await dependencies.listVersions(familyId));
  const ids = [...new Set(versions.map((version) => version.app_id))];
  const result: FamilyDeactivationResult = { cancelled: false, removed: 0, failed: 0, remaining: 0 };
  // An already-empty server needs neither a destructive prompt nor a delete request.
  if (ids.length === 0) return result;
  // Confirmation covers this exact snapshot; subsequently published versions are never silently added.
  if (!await dependencies.confirm(ids.length)) return { ...result, cancelled: true, remaining: ids.length };
  // Serial cleanup bounds load and still attempts the remaining reviewed versions after an individual failure.
  for (const id of ids) {
    try {
      await dependencies.deactivate(id);
      result.removed++;
    } catch {
      result.failed++;
    }
  }
  try {
    // Re-read each family to expose concurrent publication or failed cleanup instead of claiming it is stopped.
    for (const familyId of families) result.remaining! += (await dependencies.listVersions(familyId)).length;
  } catch {
    // A verification failure leaves the server's final state unknown, even if every deletion request succeeded.
    result.remaining = null;
  }
  return result;
}
