import assert from "node:assert/strict";
import test from "node:test";
import { APIRequestError } from "./authorization-error.ts";
import { syncBillingAfterReturn } from "./billing-sync.ts";

// Returning from payment must recover from a webhook holding the billing account lock.
test("billing return retries contention until canonical reconciliation completes", async () => {
  let attempts = 0;
  const waits = [];
  // Model the observed 409 race before the webhook finishes committing paid access.
  const sync = async () => {
    attempts++;
    // Only the first two attempts collide; the next request can safely reconcile the settled invoice.
    if (attempts < 3) throw new APIRequestError(409, { message: "a billing request is already in progress" });
  };
  // Capture waits without slowing the regression test.
  const wait = async (delay) => { waits.push(delay); };
  await syncBillingAfterReturn(sync, wait);
  assert.equal(attempts, 3);
  assert.deepEqual(waits, [500, 1000]);
});

// A provider error can have an ambiguous outcome and must retain the existing no-retry behavior.
test("billing return does not retry non-conflict failures", async () => {
  let attempts = 0;
  const error = new APIRequestError(503, { message: "billing is temporarily unavailable" });
  // Fail before any simulated success so accidental retries remain visible.
  const sync = async () => { attempts++; throw error; };
  // A non-conflict error must not even schedule a delayed retry.
  const wait = async () => { assert.fail("unexpected retry"); };
  await assert.rejects(syncBillingAfterReturn(sync, wait), error);
  assert.equal(attempts, 1);
});

// Persistent contention must release the busy UI with its actionable error instead of looping forever.
test("billing return bounds repeated contention", async () => {
  let attempts = 0;
  const error = new APIRequestError(409, { message: "a billing request is already in progress" });
  // Keep the lock occupied for every request to exercise the retry ceiling.
  const sync = async () => { attempts++; throw error; };
  // Elide wall-clock time while preserving the retry sequence.
  const wait = async () => {};
  await assert.rejects(syncBillingAfterReturn(sync, wait), error);
  assert.equal(attempts, 5);
});
