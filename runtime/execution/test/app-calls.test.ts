import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import vm from "node:vm";
import test from "node:test";
import { fused, type JsonObject } from "../src/index";
import { buildExecutionBundle } from "../src/bundle";

// Malformed calls must fail before any host, network or execution result is needed.
test("app calls reject URLs and non-object inputs", async () => {
  await assert.rejects(fused.callApp("https://other/app", {}), /alias/);
  await assert.rejects(fused.callApp("child", [] as unknown as JsonObject), /JSON object/);
  await assert.rejects(fused.callApp("child", null as unknown as JsonObject), /JSON object/);
});

// The real compiler must expose app composition even when no provider operations are selected.
test("compiled app calls preserve the child envelope and exact alias through the recorded bridge", async () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-app-calls-"));
  try {
    const entryFile = path.join(directory, "app.ts");
    fs.writeFileSync(entryFile, `import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
import {fused} from "@fused/operations";
export default buildUnifiedApp({input:z.object({value:z.string()}),output:z.object({value:z.string()}),
// The child owns its contract; the parent checks success before validating the returned output.
async execute({input}) {
 const result=await fused.callApp("child", input);
 // A failed child must not be interpreted as a successful empty result.
 if(result.status!=="succeeded") throw Error("child failed");
 return z.object({value:z.string()}).parse(result.output);
}});`);
    const bundle = await buildExecutionBundle({ entryFile, selectedOperations: [] });
    const requests: unknown[] = [];
    let status = "succeeded";
    const context = vm.createContext({ __fusedHost: {
      // Inspect the complete host payload so provider selectors or credentials cannot leak into delegation.
      async fetch(raw: string) {
        requests.push(JSON.parse(raw));
        return JSON.stringify({ status, executionId: "child-run", output: { value: "child output" } });
      },
    } });
    vm.runInContext(bundle.code, context);
    const result = await context.FusedUnifiedApp.execute({ input: { value: "input" } });
    assert.equal(result.value, "child output");
    assert.deepEqual(requests, [{ unifiedApp: "child", input: { value: "input" } }]);
    status = "failed";
    await assert.rejects(context.FusedUnifiedApp.execute({ input: { value: "input" } }), /child failed/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
