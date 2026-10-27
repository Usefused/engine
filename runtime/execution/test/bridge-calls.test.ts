import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import vm from "node:vm";
import test from "node:test";
import { fused } from "../src/index";
import { buildExecutionBundle } from "../src/bundle";
import { operationAliases } from "../src/aliases";

// Malformed aliases and facade collisions must fail before a local capability can be invoked.
test("Bridge calls retain a reserved facade name and reject routing URLs", async () => {
 await assert.rejects(fused.callBridge("https://other/bridge",20),/alias/);
 assert.notEqual(operationAliases(["callBridge"],true).get("callBridge"),"callBridge");
});
// The real compiler must preserve scalar local payloads through the recorded host-effect channel.
test("compiled Bridge calls use only the selected alias and JSON input", async () => {
 const directory=fs.mkdtempSync(path.join(os.tmpdir(),"fused-bridge-call-"));
 try {
  const entryFile=path.join(directory,"app.ts");
  fs.writeFileSync(entryFile,`import * as z from "zod/mini";
import {buildUnifiedApp} from "@fused/unified-app";
import {fused} from "@fused/operations";
export default buildUnifiedApp({input:z.object({volume:z.number()}),output:z.object({volume:z.number()}),
// One local effect belongs to the existing host transcript.
async execute({input}) {return {volume:z.number().parse(await fused.callBridge("volume",input.volume))};}});`);
  const bundle=await buildExecutionBundle({entryFile,selectedOperations:[]});
  const requests:unknown[]=[];
  const context=vm.createContext({__fusedHost:{
   /** Record exactly what crosses the Engine boundary, without a network or local device. */
   async fetch(raw:string) {
    // Admission's snapshot read precedes ordinary recorded effects.
    if(JSON.parse(raw).snapshot==="load") return "null";
    requests.push(JSON.parse(raw));return "20";
   },
  }});
  vm.runInContext(bundle.code,context);
  const result=await context.FusedUnifiedApp.execute({input:{volume:20}});
  assert.equal(result.volume,20);assert.deepEqual(requests,[{bridge:"volume",input:20}]);
 } finally {fs.rmSync(directory,{recursive:true,force:true});}
});
