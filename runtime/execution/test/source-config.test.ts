import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import vm from "node:vm";
import { buildExecutionBundle } from "../src/bundle";

// Inline metadata must typecheck without granting bindings or changing the hosted execution contract.
test("buildUnifiedApp accepts inline config as inert deployment metadata", async () => {
 const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-inline-config-"));
 try {
  const entryFile = path.join(directory,"app.ts");
  fs.writeFileSync(entryFile, `import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
export default buildUnifiedApp({config:{"name":"customer","version":"1.0.0","bucket":"default","services":{"unselected":{"version":"v1","operations":["createCustomer"]}}},input:z.object({}),output:z.object({ready:z.boolean()}),execute(){return {ready:true};}});`);
  const bundle = await buildExecutionBundle({entryFile,selectedOperations:[]});
  const sandbox: Record<string,unknown> = {};
  vm.runInNewContext(bundle.code,sandbox,{timeout:5000});
  const manifest = sandbox.FusedExecutionManifest as {selectedOperations:unknown[]};
  assert.equal(manifest.selectedOperations.length,0);
  assert.equal(sandbox.config,undefined);
 } finally {
  // Test source files are disposable; compiled app bundles contain no host filesystem access.
  fs.rmSync(directory,{recursive:true,force:true});
 }
});

// A retired export must fail admission even when a valid current config property is also present.
test("compiler rejects fusedConfig without any compatibility fallback", async () => {
 const directory = fs.mkdtempSync(path.join(os.tmpdir(), "fused-retired-config-"));
 try {
  const entryFile=path.join(directory,"app.ts");
  fs.writeFileSync(entryFile, `import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
export const fusedConfig = {"name":"old"};
export default buildUnifiedApp({config:{"name":"current"},input:z.object({}),output:z.object({}),execute(){return {};}});`);
  await assert.rejects(buildExecutionBundle({entryFile,selectedOperations:[]}),/fusedConfig is no longer supported/);
 } finally {
  // Rejection occurs before app evaluation and leaves only these disposable fixture files.
  fs.rmSync(directory,{recursive:true,force:true});
 }
});
