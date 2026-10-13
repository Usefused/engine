import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement, useState } from 'react';
import { JSDOM } from 'jsdom';
import * as fields from './unified-app-input-fields.ts';
import * as select from '../components/forms/Select.ts';
const require = createRequire(import.meta.url);
const compiled = ts.transpileModule(readFileSync(new URL('../components/apps/UnifiedAppInput.tsx', import.meta.url), 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
const exports = {};
// Resolve the schema helper without replacing production field extraction.
new Function('require', 'exports', compiled)(name => name === '~/lib/unified-app-input-fields' ? fields : name === '~/components/forms/Select' ? select : require(name), exports);
const { UnifiedAppInput } = exports;
// Chip edits must preserve JSON types and never commit malformed or unfinished drafts.
test('input chips preserve types, nested values, special keys and raw drafts', async () => {
 const dom = new JSDOM('<div id="root"></div>');
 const previous = {window:globalThis.window,document:globalThis.document,act:globalThis.IS_REACT_ACT_ENVIRONMENT};
 globalThis.window=dom.window;globalThis.document=dom.window.document;globalThis.IS_REACT_ACT_ENVIRONMENT=true;
 // Initialize React DOM after the browser environment so focus uses its standard input events.
 const {createRoot}=await import('react-dom/client');const {Simulate}=await import('react-dom/test-utils');
 let text='{"count":0,"enabled":false,"text":"false","nested":{"list":[null,2]}}';let editing=false;
 /** Models the invocation's committed payload and unfinished-edit execution lock. */
 function Harness(){const [value,setValue]=useState(text);return createElement(UnifiedAppInput,{value,disabled:false,onEditingChange:next=>{editing=next;},onChange:next=>{text=next;setValue(next);}});}
 const root=createRoot(document.getElementById('root'));
 /** Uses accessible action names to exercise the actual chip buttons. */
 async function click(name){const el=[...document.querySelectorAll('button')].find(el=>(el.getAttribute('aria-label')||el.textContent)===name);assert.ok(el,name);assert.equal(el.disabled,false);await act(async()=>Simulate.click(el));}
 /** Types into labeled fields through their React handlers. */
 async function change(label,value){const el=[...document.querySelectorAll('label')].find(el=>el.textContent.startsWith(label));await act(async()=>Simulate.change(document.getElementById(el.htmlFor),{target:{value}}));}
 try{
  await act(async()=>root.render(createElement(Harness)));
  await click('Edit count');assert.equal(editing,true);await change('Value','12');await click('Save field');assert.equal(editing,false);
  assert.deepEqual(JSON.parse(text),{count:12,enabled:false,text:'false',nested:{list:[null,2]}});
  await click('Edit text');await change('Value','true');await click('Save field');assert.equal(JSON.parse(text).text,'true');
  await click('Add field');await change('Field','__proto__');await change('Value','ordinary data');await click('Save field');assert.equal(JSON.parse(text).__proto__,'ordinary data');
  await click('Add field');await change('Field','count');await click('Save field');assert.match(document.querySelector('[role="alert"]').textContent,/already exists/);assert.equal(editing,true);await click('Cancel');
  await click('Edit count');await change('Value','1e999');await click('Save field');assert.equal(JSON.parse(text).count,12);await click('Cancel');
  await click('Remove enabled');assert.equal(Object.hasOwn(JSON.parse(text),'enabled'),false);
  await click('JSON');await act(async()=>Simulate.change(document.querySelector('textarea'),{target:{value:'{unfinished'}}));assert.equal(text,'{unfinished');assert.equal([...document.querySelectorAll('button')].find(el=>el.textContent==='Fields').disabled,true);
  await act(async()=>Simulate.change(document.querySelector('textarea'),{target:{value:'{"items":[1,false,null]}'}}));await click('Fields');await click('Edit items');await click('Save field');assert.deepEqual(JSON.parse(text),{items:[1,false,null]});
 }finally{await act(async()=>root.unmount());dom.window.close();globalThis.window=previous.window;globalThis.document=previous.document;globalThis.IS_REACT_ACT_ENVIRONMENT=previous.act;}
});

// Inline values update immediately, while invalid local text must never execute an older payload.
test('schema chips edit inline, preserve types, and track independent invalid drafts', async () => {
 const dom=new JSDOM('<div id="root"></div>');
 const previous={window:globalThis.window,document:globalThis.document,act:globalThis.IS_REACT_ACT_ENVIRONMENT};
 globalThis.window=dom.window;globalThis.document=dom.window.document;globalThis.IS_REACT_ACT_ENVIRONMENT=true;
 const {createRoot}=await import('react-dom/client');const {Simulate}=await import('react-dom/test-utils');
 const schema={type:'object',required:['count'],properties:{count:{type:'integer'},status:{type:'string',enum:['','open','closed']},enabled:{type:'boolean'},note:{type:'string'},items:{type:'array'}}};
 let text='{}';let blocked=false;
 /** Retains the emitted JSON separately from the incomplete-text execution lock. */
 function Harness(){const [value,setValue]=useState(text);return createElement(UnifiedAppInput,{schema,value,disabled:false,onEditingChange:next=>{blocked=next;},onChange:next=>{text=next;setValue(next);}});}
 const root=createRoot(document.getElementById('root'));
 /** Selects visible actions by their accessible name, including clearing a malformed chip. */
 async function click(name){const el=[...document.querySelectorAll('button')].find(el=>(el.getAttribute('aria-label')||el.textContent)===name);assert.ok(el,name);assert.equal(el.disabled,false);await act(async()=>Simulate.click(el));}
 /** Types directly inside the named chip without opening or saving an editor. */
 async function change(name,value){await act(async()=>Simulate.change(document.querySelector(`[aria-label="${name}"]`),{target:{value}}));}
 try{
  await act(async()=>root.render(createElement(Harness)));
  assert.equal(document.querySelectorAll('textarea').length,3);assert.equal(text,'{}');assert.doesNotMatch(document.body.textContent,/Add field|Save field/);
  await change('count','7');assert.deepEqual(JSON.parse(text),{count:7});assert.equal(blocked,false);
  await change('note','false');assert.equal(JSON.parse(text).note,'false');
  await change('status','0');assert.equal(JSON.parse(text).status,'');
  await change('enabled','1');assert.equal(JSON.parse(text).enabled,false);
  await change('count','-');assert.equal(blocked,true);assert.equal(JSON.parse(text).count,7);assert.equal(document.querySelector('[aria-label="count"]').value,'-');
  await change('items','[');await change('count','8');assert.equal(blocked,true,'another invalid draft must keep execution locked');
  await change('items','[1,false,null]');assert.equal(blocked,false);assert.deepEqual(JSON.parse(text).items,[1,false,null]);
  await change('count','1e999');assert.equal(blocked,true);await act(async()=>Simulate.keyDown(document.querySelector('[aria-label="count"]'),{key:'Escape',preventDefault(){}}));assert.equal(blocked,false);assert.equal(document.querySelector('[aria-label="count"]').value,'8');
  await change('count','');assert.equal(Object.hasOwn(JSON.parse(text),'count'),false);assert.equal(blocked,false,'required fields are enforced by full-contract validation');
  await change('items','[');await click('Clear items');assert.equal(blocked,false);assert.equal(Object.hasOwn(JSON.parse(text),'items'),false);
  await change('note','');assert.equal(JSON.parse(text).note,'');await click('Clear note');assert.equal(Object.hasOwn(JSON.parse(text),'note'),false);
  await click('JSON');await act(async()=>Simulate.change(document.querySelector('textarea'),{target:{value:'{"count":42}'}}));await click('Fields');assert.equal(document.querySelector('[aria-label="count"]').value,'42','raw JSON changes cannot be hidden by stale inline text');
 }finally{await act(async()=>root.unmount());dom.window.close();globalThis.window=previous.window;globalThis.document=previous.document;globalThis.IS_REACT_ACT_ENVIRONMENT=previous.act;}
});

// Local definitions resolve without network access; ambiguous roots retain JSON instead of guessing a branch.
test('schema field extraction supports references and honest JSON fallbacks',()=>{
 const schema={type:'object',required:['name'],properties:{name:{$ref:'#/$defs/name'},choice:{anyOf:[{type:'string'},{type:'null'}]}},$defs:{name:{type:'string',description:'Customer name'}}};
 assert.deepEqual(fields.unifiedInputFields(schema).map(({name,type,required})=>({name,type,required})),[{name:'name',type:'string',required:true},{name:'choice',type:'json',required:false}]);
 assert.equal(fields.unifiedInputFields(schema)[0].description,'Customer name');
 assert.equal(fields.unifiedInputFields({$ref:'#'}),null);
 assert.equal(fields.unifiedInputFields({oneOf:[schema]}),null);
 assert.equal(fields.unifiedInputFields({type:'array'}),null);
});
