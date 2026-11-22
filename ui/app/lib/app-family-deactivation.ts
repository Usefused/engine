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

export interface FamilyDeletionDependencies {
  listVersions: (familyId: string) => Promise<AppVersionIdentity[]>;
  confirm: (summary: { familyCount: number; versionCount: number }) => Promise<boolean>;
  deactivate: (appId: string) => Promise<unknown>;
  archiveFamily: (familyId: string) => Promise<unknown>;
}

export interface FamilyDeletionResult {
  cancelled: boolean;
  deletedFamilies: string[];
  failedFamilies: string[];
  deactivatedVersions: number;
  failedVersions: number;
}

/** Retains only selected app families still present on the reloaded catalogue page. */
export function pruneSelectedFamilyIds(selectedFamilyIds: string[], loadedFamilyIds: string[]): string[] {
  const loaded = new Set(loadedFamilyIds);
  // Keep surviving families checked across deactivation reloads while dropping deleted or paged-out rows.
  return selectedFamilyIds.filter((id) => loaded.has(id));
}

/** Deactivates any remaining immutable versions for each selected app family and then archives the family. */
export async function deleteAppFamilies(familyIds: string[], dependencies: FamilyDeletionDependencies): Promise<FamilyDeletionResult> {
  const families = [...new Set(familyIds)];
  const empty: FamilyDeletionResult = { cancelled: false, deletedFamilies: [], failedFamilies: [], deactivatedVersions: 0, failedVersions: 0 };
  // Empty selections require neither version discovery nor a destructive prompt.
  if (families.length === 0) return empty;
  const versionsByFamily = new Map<string, string[]>();
  let versionCount = 0;
  // Discover every selected family's remaining versions upfront so discovery failures abort before mutation.
  for (const familyId of families) {
    const versions = [...new Set((await dependencies.listVersions(familyId)).map((version) => version.app_id))];
    versionsByFamily.set(familyId, versions);
    versionCount += versions.length;
  }
  // Confirm the exact family and version counts once before performing any irreversible cleanup.
  if (!await dependencies.confirm({ familyCount: families.length, versionCount })) {
    return { ...empty, cancelled: true };
  }
  const result: FamilyDeletionResult = { ...empty, deletedFamilies: [], failedFamilies: [] };
  // Process each family serially so partial failures remain isolated to their owning family.
  for (const familyId of families) {
    const versionIds = versionsByFamily.get(familyId) ?? [];
    let familyDeactivationFailed = false;
    for (const versionId of versionIds) {
      try {
        await dependencies.deactivate(versionId);
        result.deactivatedVersions++;
      } catch {
        result.failedVersions++;
        familyDeactivationFailed = true;
      }
    }
    // Engine requires every immutable version to be deactivated before releasing the family name.
    if (familyDeactivationFailed) {
      result.failedFamilies.push(familyId);
      continue;
    }
    try {
      await dependencies.archiveFamily(familyId);
      result.deletedFamilies.push(familyId);
    } catch {
      result.failedFamilies.push(familyId);
    }
  }
  return result;
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
