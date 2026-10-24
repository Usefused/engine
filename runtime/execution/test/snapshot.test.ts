import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { z } from "zod";
import { buildUnifiedApp, runUnifiedApp, snapshot, validateRecoveryPolicy } from "../src/index";
import { buildExecutionBundle } from "../src/bundle";

// Hosted bundles must restore Engine state on every invocation without trusting caller-supplied progress.
test("compiled execution automatically restores snapshots in independent interpreters", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-snapshot-context-"));
  try {
    const entryFile = path.join(directory, "app.ts");
    fs.writeFileSync(entryFile, `import * as z from "zod/mini";
import {buildUnifiedApp,snapshot} from "@fused/unified-app";
export default buildUnifiedApp({input:z.object({}),output:z.object({cursor:z.number()}),
// A replacement receives its committed continuation without loading it in authored code.
async execute({snapshot:restored}) {
 const saved=z.nullable(z.object({cursor:z.number()})).parse(restored);
 // Fresh work fails only after its checkpoint has been acknowledged.
 if(saved===null){await snapshot.save({cursor:7});throw new Error("deliberate failure");}
 return saved;
}});`);
    const bundle = await buildExecutionBundle({ entryFile, selectedOperations: [] });
    let stored = "null";
    const requests: unknown[] = [];
    const host = {
      // Model durable storage shared by otherwise isolated worker instances.
      async fetch(raw: string) {
        const request = JSON.parse(raw);
        requests.push(request);
        // Reads expose only the current run's persisted value.
        if (request.snapshot === "load") return stored;
        assert.equal(request.snapshot, "save");
        stored = JSON.stringify(request.state);
        return "null";
      },
    };
    // Each invocation gets a new interpreter, matching replacement-worker isolation.
    const invoke = async () => {
      const context = vm.createContext({ __fusedHost: host });
      vm.runInContext(bundle.code, context);
      return context.FusedUnifiedApp.execute({ input: {}, snapshot: { cursor: 999 } });
    };
    await assert.rejects(invoke(), /deliberate failure/);
    assert.deepEqual(JSON.parse(JSON.stringify(await invoke())), { cursor: 7 });
    assert.deepEqual(requests, [{ snapshot: "load" }, { snapshot: "save", state: { cursor: 7 } }, { snapshot: "load" }]);
    stored = "null";
    await assert.rejects(invoke(), /deliberate failure/);
    assert.equal(requests.length, 5);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});

// Failed restoration must stop before authored effects, while scalar JSON checkpoints remain valid.
test("runtime restores JSON snapshots and fails closed before execute", async () => {
  let stored = "false";
  let entered = 0;
  const app = buildUnifiedApp({
    input: z.object({}),
    output: z.object({ value: z.unknown() }),
    // Count entry to distinguish restoration failures from authored failures.
    execute({ snapshot }) { entered++; return { value: snapshot }; },
  });
  globalThis.__fusedHost = {
    // Only the automatic restore request is valid before authored execution.
    async fetch(raw) { assert.deepEqual(JSON.parse(raw), { snapshot: "load" }); return stored; },
    // Snapshot restoration cannot fall back to buffered data.
    async dbGet() { throw new Error("unexpected data read"); },
    // Snapshot restoration must remain read-only.
    async dbSet() { throw new Error("unexpected data write"); },
  };
  try {
    // Falsy JSON values are checkpoints, not missing state.
    for (const value of [false, 0, "", [], null]) {
      stored = JSON.stringify(value);
      assert.deepEqual(await runUnifiedApp(app, {}), { value });
    }
    stored = "invalid JSON";
    await assert.rejects(runUnifiedApp(app, {}), SyntaxError);
    // An unavailable read cannot silently reset a retry to fresh work.
    globalThis.__fusedHost.fetch = async () => { throw new Error("snapshot unavailable"); };
    await assert.rejects(runUnifiedApp(app, {}), /snapshot unavailable/);
    assert.equal(entered, 5);
  } finally {
    globalThis.__fusedHost = undefined;
  }
});

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
