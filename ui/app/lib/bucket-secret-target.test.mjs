import assert from "node:assert/strict";
import test from "node:test";
import {readFileSync} from "node:fs";
import ts from "typescript";

/** Executes the production lookup with a recorded metadata-only transport. */
function client(read){
 const source=readFileSync(new URL('./bucket-secret-target.ts',import.meta.url),'utf8');
 const code=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022}}).outputText;
 const exports={};
 new Function('require','exports',code)(()=>({api:{mcpGraphql:read}}),exports);
 return exports;
}
// A deep link must find the exact namespaced secret even when it is beyond the first page.
test('locates an exact bucket variable across metadata pages',async()=>{
 const calls=[];
 const expected={credential_type:'bucket_secret',key_name:'secret:signing_key',expires_at:'2027-01-01T00:00:00Z'};
 const api=client(async(query,variables)=>{
  calls.push(variables);
  assert.doesNotMatch(query,/\bvalue\b/);
  // Earlier pages may contain similarly named provider credentials, which are not this variable.
  return {secretMetaPage:variables.offset===0?{total:101,items:Array(100).fill({credential_type:'api_key',key_name:'secret:signing_key'})}:{total:101,items:[expected]}};
 });
 assert.deepEqual(await api.readNamedBucketSecret('bucket','signing_key'),expected);
 assert.deepEqual(calls.map(x=>x.offset),[0,100]);
});
// Missing and denied metadata must not open a writable creation form.
test('distinguishes missing variables from denied metadata',async()=>{
 assert.equal(await client(async()=>({secretMetaPage:{total:0,items:[]}})).readNamedBucketSecret('bucket','missing'),null);
 await assert.rejects(client(async()=>{throw new Error('permission denied');}).readNamedBucketSecret('bucket','private'),/permission denied/);
});
