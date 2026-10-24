import assert from "node:assert/strict";
import test from "node:test";
import { snapshot, validateRecoveryPolicy } from "../src/index";

// Snapshot helpers must use the recorded host channel and propagate uncertain saves.
test("snapshot uses durable host calls and propagates lost acknowledgements", async () => {
  const requests: unknown[] = [];
  globalThis.__fusedHost = {
    // Simulate one persisted continuation and an unavailable save acknowledgement.
    async fetch(raw) {
      const request = JSON.parse(raw);
      requests.push(request);
      // Reads return saved JSON; writes must not pretend persistence succeeded.
      if (request.snapshot === "load") return '{"cursor":7}';
      throw new Error("save acknowledgement lost");
    },
    // Buffered data must remain independent of crash-safe snapshots.
    async dbGet() { throw new Error("unexpected buffered data access"); },
    // A checkpoint cannot accidentally route to terminal-only data storage.
    async dbSet() { throw new Error("unexpected buffered data write"); },
  };
  try {
    assert.deepEqual(await snapshot.load(), { cursor: 7 });
    await assert.rejects(snapshot.save({ cursor: 8 }), /acknowledgement lost/);
    assert.deepEqual(requests, [{ snapshot: "load" }, { snapshot: "save", state: { cursor: 8 } }]);
    await assert.rejects(snapshot.save({ bad: undefined } as never), /JSON/);
  } finally {
    globalThis.__fusedHost = undefined;
  }
});

// Recovery remains manual by default, and every policy has finite retry and delay limits.
test("recovery policy defaults to manual and bounds both trigger modes", () => {
  assert.deepEqual(validateRecoveryPolicy(), { mode: "manual", maxAttempts: 3, backoffSeconds: 5 });
  assert.equal(validateRecoveryPolicy({ mode: "automatic" }).mode, "automatic");
  // Invalid budgets cannot create an unbounded background execution loop.
  for (const maxAttempts of [0, 11, 1.5, Infinity]) {
    assert.throws(() => validateRecoveryPolicy({ maxAttempts }), /maxAttempts/);
  }
  assert.throws(() => validateRecoveryPolicy({ backoffSeconds: 0 }), /backoffSeconds/);
});
