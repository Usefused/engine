import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { deactivateFamilyVersions, deleteAppFamilies, pruneSelectedFamilyIds } from "./app-family-deactivation.ts";

/** Models independent families and records the exact immutable versions submitted for cleanup. */
function fixture(initial, { approved = true, fail = [], publishAfter = false, readFailureAt = 0 } = {}) {
  const families = new Map(Object.entries(initial).map(([id, versions]) => [id, [...versions]]));
  const calls = [];
  const confirmations = [];
  let reads = 0;
  return { families, calls, confirmations, dependencies: {
    /** Returns the full family snapshot and optionally simulates unavailable discovery. */
    listVersions: async (familyId) => {
      reads++;
      // Inject the failure at a chosen boundary to distinguish discovery from post-cleanup verification.
      if (reads === readFailureAt) throw new Error("discovery unavailable");
      return families.get(familyId).map((app_id) => ({ app_id }));
    },
    /** Records the review count and can publish a new version after the snapshot was shown. */
    confirm: async (count) => {
      confirmations.push(count);
      // A concurrent publication was not in the reviewed snapshot and must survive this action.
      if (publishAfter) families.values().next().value.push("concurrent");
      return approved;
    },
    /** Removes a requested version only from its owning family, retaining explicit simulated failures. */
    deactivate: async (appId) => {
      calls.push(appId);
      // A rejected cleanup must remain present and cannot count as a successful removal.
      if (fail.includes(appId)) throw new Error("cleanup failed");
      for (const [familyId, versions] of families) families.set(familyId, versions.filter((id) => id !== appId));
    },
  } };
}

// Cover cancellation before any irreversible request, even with multiple runnable versions.
test("cancel leaves every version untouched", async () => {
  const f = fixture({ server: ["old", "latest"] }, { approved: false });
  const result = await deactivateFamilyVersions(["server"], f.dependencies);
  assert.equal(result.cancelled, true);
  assert.deepEqual(f.confirmations, [2]);
  assert.deepEqual(f.calls, []);
  assert.deepEqual(f.families.get("server"), ["old", "latest"]);
});

// Exercise a server larger than the catalogue page and isolate unselected families.
test("all versions of selected servers use exact-version cleanup", async () => {
  const versions = Array.from({ length: 103 }, (_, index) => `version-${index}`);
  const f = fixture({ server: versions, second: ["second-v1"], untouched: ["keep"] });
  const result = await deactivateFamilyVersions(["server", "second", "server"], f.dependencies);
  assert.deepEqual(result, { cancelled: false, removed: 104, failed: 0, remaining: 0 });
  assert.deepEqual(f.confirmations, [104]);
  assert.deepEqual(f.calls, [...versions, "second-v1"]);
  assert.deepEqual(f.families.get("untouched"), ["keep"]);
});

// A failure must not hide surviving versions or prevent cleanup of later reviewed siblings.
test("partial failure is reported accurately", async () => {
  const f = fixture({ server: ["old", "broken", "latest"] }, { fail: ["broken"] });
  const result = await deactivateFamilyVersions(["server"], f.dependencies);
  assert.deepEqual(result, { cancelled: false, removed: 2, failed: 1, remaining: 1 });
  assert.deepEqual(f.families.get("server"), ["broken"]);
});

// Newly published versions require a separate review, not a silently expanding destructive loop.
test("concurrent publication remains and prevents an all-stopped result", async () => {
  const f = fixture({ server: ["old"] }, { publishAfter: true });
  const result = await deactivateFamilyVersions(["server"], f.dependencies);
  assert.equal(result.remaining, 1);
  assert.deepEqual(f.calls, ["old"]);
  assert.deepEqual(f.families.get("server"), ["concurrent"]);
});

// All selected families must be discovered successfully before any destructive work begins.
test("discovery failure aborts before confirmation or deletion", async () => {
  const f = fixture({ first: ["one"], second: ["two"] }, { readFailureAt: 2 });
  await assert.rejects(deactivateFamilyVersions(["first", "second"], f.dependencies), /discovery unavailable/);
  assert.deepEqual(f.calls, []);
  assert.deepEqual(f.confirmations, []);
});

// Successful requests alone cannot prove that the server has no remaining executable versions.
test("verification failure is an unknown final state", async () => {
  const f = fixture({ server: ["one"] }, { readFailureAt: 2 });
  const result = await deactivateFamilyVersions(["server"], f.dependencies);
  assert.deepEqual(result, { cancelled: false, removed: 1, failed: 0, remaining: null });
});

// Empty retained server identities need no confirmation and must not be deleted automatically.
test("empty server does not dispatch cleanup", async () => {
  const f = fixture({ server: [] });
  const result = await deactivateFamilyVersions(["server"], f.dependencies);
  assert.deepEqual(result, { cancelled: false, removed: 0, failed: 0, remaining: 0 });
  assert.deepEqual(f.confirmations, []);
  assert.deepEqual(f.calls, []);
});

/** Models multi-family deletion including version deactivation, archive failures, and discovery outages. */
function deletionFixture(initial, { approved = true, failVersions = [], failArchives = [], readFailureAt = 0 } = {}) {
  const families = new Map(Object.entries(initial).map(([id, versions]) => [id, [...versions]]));
  const deactivated = [];
  const archived = [];
  const confirmations = [];
  let reads = 0;
  return {
    families,
    deactivated,
    archived,
    confirmations,
    dependencies: {
      /** Lists remaining versions for one family, optionally failing discovery. */
      listVersions: async (familyId) => {
        reads++;
        // Inject a failure at a specific discovery call to verify abort-before-mutation semantics.
        if (reads === readFailureAt) throw new Error("discovery unavailable");
        return (families.get(familyId) ?? []).map((app_id) => ({ app_id }));
      },
      /** Records confirmation summary and returns the simulated operator choice. */
      confirm: async (summary) => {
        confirmations.push(summary);
        return approved;
      },
      /** Simulates per-version deactivation with optional targeted failures. */
      deactivate: async (appId) => {
        deactivated.push(appId);
        // Failed version cleanup leaves the version present in its owning family.
        if (failVersions.includes(appId)) throw new Error("deactivate failed");
        for (const [familyId, versions] of families) {
          families.set(familyId, versions.filter((id) => id !== appId));
        }
      },
      /** Simulates family archiving only when all versions are gone and not configured to fail. */
      archiveFamily: async (familyId) => {
        archived.push(familyId);
        // Mirror Engine's conflict rule if any version remains unremoved.
        if ((families.get(familyId) ?? []).length > 0) throw new Error("deactivate every app version before deleting the app");
        if (failArchives.includes(familyId)) throw new Error("archive failed");
        families.delete(familyId);
      },
    },
  };
}

// Verifies bulk deletion handles already-deactivated families, active families, and mixed selections cleanly.
test("deleteAppFamilies deactivates remaining versions and archives both active and versionless families", async () => {
  const f = deletionFixture({ activeApp: ["v1", "v2"], alreadyDeactivated: [], untouched: ["keep"] });
  const result = await deleteAppFamilies(["activeApp", "alreadyDeactivated", "activeApp"], f.dependencies);
  assert.deepEqual(result, {
    cancelled: false,
    deletedFamilies: ["activeApp", "alreadyDeactivated"],
    failedFamilies: [],
    deactivatedVersions: 2,
    failedVersions: 0,
  });
  assert.deepEqual(f.confirmations, [{ familyCount: 2, versionCount: 2 }]);
  assert.deepEqual(f.deactivated, ["v1", "v2"]);
  assert.deepEqual(f.archived, ["activeApp", "alreadyDeactivated"]);
  assert.deepEqual(f.families.get("untouched"), ["keep"]);
});

// Verifies bulk deletion of already-deactivated apps skips deactivation and archives each selected family.
test("deleteAppFamilies deletes multiple already-deactivated apps directly", async () => {
  const f = deletionFixture({ first: [], second: [] });
  const result = await deleteAppFamilies(["first", "second"], f.dependencies);
  assert.deepEqual(result, {
    cancelled: false,
    deletedFamilies: ["first", "second"],
    failedFamilies: [],
    deactivatedVersions: 0,
    failedVersions: 0,
  });
  assert.deepEqual(f.confirmations, [{ familyCount: 2, versionCount: 0 }]);
  assert.deepEqual(f.deactivated, []);
  assert.deepEqual(f.archived, ["first", "second"]);
});

// Verifies cancelling deletion leaves every version and family untouched.
test("deleteAppFamilies cancellation performs no mutations", async () => {
  const f = deletionFixture({ app: ["v1"] }, { approved: false });
  const result = await deleteAppFamilies(["app"], f.dependencies);
  assert.equal(result.cancelled, true);
  assert.deepEqual(f.deactivated, []);
  assert.deepEqual(f.archived, []);
  assert.deepEqual(f.families.get("app"), ["v1"]);
});

// Verifies a failing version blocks archiving only for its owning family while sibling families still delete.
test("deleteAppFamilies isolates version and archive failures per family", async () => {
  const f = deletionFixture(
    { brokenVersion: ["ok", "bad"], archiveFails: ["v3"], healthy: [] },
    { failVersions: ["bad"], failArchives: ["archiveFails"] },
  );
  const result = await deleteAppFamilies(["brokenVersion", "archiveFails", "healthy"], f.dependencies);
  assert.deepEqual(result, {
    cancelled: false,
    deletedFamilies: ["healthy"],
    failedFamilies: ["brokenVersion", "archiveFails"],
    deactivatedVersions: 2,
    failedVersions: 1,
  });
  assert.deepEqual(f.archived, ["archiveFails", "healthy"]);
});

// Verifies discovery errors abort before prompting or mutating any selected family.
test("deleteAppFamilies aborts before confirmation if discovery fails", async () => {
  const f = deletionFixture({ first: ["v1"], second: ["v2"] }, { readFailureAt: 2 });
  await assert.rejects(deleteAppFamilies(["first", "second"], f.dependencies), /discovery unavailable/);
  assert.deepEqual(f.confirmations, []);
  assert.deepEqual(f.deactivated, []);
  assert.deepEqual(f.archived, []);
});

// Verifies selection stays intact across deactivation reloads while dropping deleted or paged-out families.
test("pruneSelectedFamilyIds preserves surviving deactivated rows and drops deleted ones", () => {
  assert.deepEqual(pruneSelectedFamilyIds(["fam-1", "fam-2", "fam-3"], ["fam-1", "fam-3", "fam-4"]), ["fam-1", "fam-3"]);
  assert.deepEqual(pruneSelectedFamilyIds(["fam-1", "fam-2"], []), []);
});

// Verifies all Fused app types gate detail-page deletion by family permission and support multi-select deletion in the catalogue.
test("all Fused app detail views and the shared catalogue wire permission-gated deletion and persistent family selection", () => {
  const menu = readFileSync(new URL("../components/apps/AppHeaderActionsMenu.tsx", import.meta.url), "utf8");
  const unifiedDetail = readFileSync(new URL("../routes/integrations.unified-apps.$id.tsx", import.meta.url), "utf8");
  const sdkDetail = readFileSync(new URL("../routes/integrations.sdks.$id.tsx", import.meta.url), "utf8");
  const mcpDetail = readFileSync(new URL("../routes/integrations.mcp_.$id.tsx", import.meta.url), "utf8");
  const catalogue = readFileSync(new URL("../routes/integrations.sdks._index.tsx", import.meta.url), "utf8");

  // Shared overflow menu hides itself when the actor has no actions and gates Delete app on canDelete.
  assert.match(menu, /if \(links\.length === 0 && !showDelete\) return null/);
  assert.match(menu, /Delete app/);

  // Every app type's detail route gates deletion on its exact family manage permission.
  assert.match(unifiedDetail, /hasResourcePermission\(access, "app\.unified_app\.manage", "APP", app\.app_family_id\)/);
  assert.match(unifiedDetail, /canDelete=\{canManage\} deleting=\{deletingApp\} onDelete=\{deleteApp\}/);
  assert.match(sdkDetail, /hasResourcePermission\(access, `app\.\$\{sdk\.delivery_mode === "api" \? "api" : "sdk"\}\.manage`, "APP", sdk\.app_family_id\)/);
  assert.match(sdkDetail, /canManageVersions && <AppHeaderActionsMenu canDelete=\{canManageVersions\} deleting=\{deletingApp\} onDelete=\{onDeleteApp\}/);
  assert.match(mcpDetail, /hasResourcePermission\(access, "app\.mcp\.manage", "APP", state\.server\?\.app_family_id \?\? ""\)/);
  assert.match(mcpDetail, /canManageVersions && <AppHeaderActionsMenu canDelete=\{canManageVersions\} deleting=\{deletingApp\} onDelete=\{onDeleteApp\}/);

  // Shared catalogue selects by app_family_id (including deactivated rows), preserves selection after deactivation, and offers bulk Delete selected.
  assert.match(catalogue, /const selectableIds = archived \? \[\] : sdks\.flatMap\(\(sdk\) => canManage\(sdk\) \? \[sdk\.app_family_id\] : \[\]\)/);
  assert.match(catalogue, /pruneSelectedFamilyIds\(previous, loaded\.map\(\(item\) => item\.app_family_id\)\)/);
  assert.match(catalogue, /await fetchSdks\(query, page, true\)/);
  assert.match(catalogue, /Delete selected \(\$\{selectedCount\}\)/);
});
