import assert from "node:assert/strict";
import test from "node:test";
import { deactivateFamilyVersions } from "./app-family-deactivation.ts";

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
