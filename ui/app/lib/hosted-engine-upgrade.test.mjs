import test from "node:test";
import assert from "node:assert/strict";
import { readUpgradeIntent, upgradeCompleted, upgradeInProgress } from "./hosted-engine-upgrade.ts";

const intent = { id: "11111111-1111-4111-8111-111111111111", expected_generation: 4, target_image: "engine:v5" };

// Lost POST responses must be recoverable from observed generation and exact release identity.
test("upgrade completion requires the requested release to be observed and ready", () => {
  const ready = { hosted: true, status: "ready", desired_generation: 5, observed_generation: 5, current_image: "engine:v5" };
  assert.equal(upgradeCompleted(ready, intent), true);
  assert.equal(upgradeCompleted({ ...ready, observed_generation: 4 }, intent), false);
  assert.equal(upgradeCompleted({ ...ready, status: "upgrading" }, intent), false);
  assert.equal(upgradeCompleted({ ...ready, current_image: "engine:v6" }, intent), false);
  assert.equal(upgradeCompleted({ ...ready, status: "failed" }, intent), false);
});

// Saved retry metadata survives reloads but malformed or unsafe generations cannot enter a new request.
test("retry metadata is bounded and restores the same operation identity", () => {
  assert.deepEqual(readUpgradeIntent(JSON.stringify(intent)), intent);
  assert.equal(readUpgradeIntent("not-json"), null);
  assert.equal(readUpgradeIntent(JSON.stringify({ ...intent, expected_generation: -1 })), null);
  assert.equal(readUpgradeIntent(JSON.stringify({ ...intent, id: "wrong" })), null);
  assert.equal(readUpgradeIntent(JSON.stringify({ ...intent, target_image: "" })), null);
  assert.equal(readUpgradeIntent(null), null);
});

// A transient restart remains pending, while a reported failure requires an explicit retry.
test("only active deployment transitions disable another update", () => {
  assert.equal(upgradeInProgress({ hosted: true, status: "upgrading" }), true);
  assert.equal(upgradeInProgress({ hosted: true, status: "ready" }), false);
  assert.equal(upgradeInProgress({ hosted: true, status: "failed" }), false);
  assert.equal(upgradeInProgress(null), false);
});
