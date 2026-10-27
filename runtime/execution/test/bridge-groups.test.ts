import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import vm from "node:vm";
import test from "node:test";
import {buildExecutionBundle} from "../src/bundle";
import {generateBridgeBindings, type SelectedBridge} from "../src/bridge-bindings";

const binding: SelectedBridge = {group:"home",alias:"pinned_volume",connection_id:"echo-show-8",operation_id:"volume.set",kind:"command",input_schema:{type:"object",properties:{level:{type:"integer"}},required:["level"],additionalProperties:false},output_schema:{type:"object",properties:{accepted:{type:"boolean"}},required:["accepted"],additionalProperties:false}};

async function compile(body: string,bindings: SelectedBridge[]=[binding]) {
 const dir=fs.mkdtempSync(path.join(os.tmpdir(),"bridge-groups-"));
 try {
  const entryFile=path.join(dir,"app.ts");
  fs.writeFileSync(entryFile,`import * as z from 'zod/mini';import {buildUnifiedApp} from '@fused/unified-app';import {bridges} from '@fused/operations';export default buildUnifiedApp({input:z.object({}),output:z.object({accepted:z.boolean()}),async execute(){${body}}});`);
  return await buildExecutionBundle({entryFile,selectedOperations:[],selectedBridges:bindings});
 } finally {fs.rmSync(dir,{recursive:true,force:true});}
}

test("grouped calls use immutable aliases and preserve typed outputs",async()=>{
 const bundle=await compile(`return await bridges.home["echo-show-8"].volume.set({level:25});`);
 const calls:unknown[]=[];
 const context=vm.createContext({__fusedHost:{async fetch(raw:string){if(JSON.parse(raw).snapshot==='load')return 'null';calls.push(JSON.parse(raw));return '{"accepted":true}';}}});
 vm.runInContext(bundle.code,context);
 assert.equal((await context.FusedUnifiedApp.execute({input:{}})).accepted,true);
 assert.deepEqual(calls,[{bridge:'pinned_volume',input:{level:25}}]);
});

test("type checking rejects unselected destinations, bad inputs and invented response fields",async()=>{
 for(const body of [
  `return await bridges.other["echo-show-8"].volume.set({level:25});`,
  `return await bridges.home["other"].volume.set({level:25});`,
  `return await bridges.home["echo-show-8"].volume.get({});`,
  `return await bridges.home["echo-show-8"].volume.set({level:"25"});`,
  `return await bridges.home["echo-show-8"].volume.set({});`,
  `return {accepted:(await bridges.home["echo-show-8"].volume.set({level:25})).imaginary};`,
 ]) await assert.rejects(compile(body),/TypeScript validation failed/);
});

test("paths reject prefix collisions and object special names cannot pollute prototypes",()=>{
 assert.throws(()=>generateBridgeBindings([binding,{...binding,alias:'another',operation_id:'volume'}]),/Conflicting/);
 const source=generateBridgeBindings([{...binding,connection_id:'__proto__'}]);
 assert.match(source,/\["__proto__"\]/);
 assert.equal(({} as Record<string,unknown>).pinned_volume,undefined);
});


test("closed empty inputs reject extra arguments before publication",async()=>{
 const empty={...binding,input_schema:{type:"object",properties:{},additionalProperties:false}};
 await assert.rejects(compile(`return await bridges.home["echo-show-8"].volume.set({level:25});`,[empty]),/TypeScript validation failed/);
 await assert.rejects(compile(`return await bridges.home["echo-show-8"].volume.set(25);`,[empty]),/TypeScript validation failed/);
 await compile(`return await bridges.home["echo-show-8"].volume.set({});`,[empty]);
});
