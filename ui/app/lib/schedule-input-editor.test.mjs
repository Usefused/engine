import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { Simulate } from 'react-dom/test-utils';
import { JSDOM } from 'jsdom';
import * as helpers from './schedule-input-validation.ts';
import * as examples from './schema-example.ts';
import * as indentation from './code-indentation.ts';
import * as differences from './code-diff.ts';
const require=createRequire(import.meta.url);

// Exercise the JSON editor and native form validity together so schema failures cannot silently submit.
test('JSON editor follows active traffic, preserves drafts and blocks invalid data types',async()=>{
 const dom=new JSDOM('<div id="root"></div>');
 const previous={window:globalThis.window,document:globalThis.document,act:globalThis.IS_REACT_ACT_ENVIRONMENT};
 globalThis.window=dom.window;globalThis.document=dom.window.document;globalThis.IS_REACT_ACT_ENVIRONMENT=true;
 let text='{}';const targets=[];
 const schema={type:'object',required:['message'],properties:{message:{type:'string'},count:{type:'integer'},enabled:{type:'boolean'}}};
 /** A decorative highlighter stub retains the real editable textarea and its native validity. */
 function Highlighter(){return null;}
 Highlighter.registerLanguage=()=>{};
 const dependencies={
  '~/lib/schedule-input-validation':helpers, '~/lib/schema-example':examples,
  '~/lib/code-indentation':indentation, '~/lib/code-diff':differences,
  '~/lib/unified-app-io':{unifiedAppContract:document=>({input:document})},
  // An older retained version must still validate against current traffic.
  '~/lib/api':{api:{appConfig:{traffic:async()=>({active_app_id:'active-version'}),openAPI:async(id)=>{targets.push(id);return schema;}}}},
  'react-syntax-highlighter/dist/esm/prism-light.js':{default:Highlighter},
 };
 /** Compiles real React components, replacing only schema transport and decorative highlighting. */
 function component(path){
  const source=readFileSync(new URL(path,import.meta.url),'utf8');
  const compiled=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
  const exports={};
  // Language token registries are presentation-only and do not alter input or validation.
  new Function('require','exports',compiled)(name=>name.includes('/languages/prism/')?{default:{}}:dependencies[name]??require(name),exports);
  return exports;
 }
 dependencies['~/components/code/TypeScriptEditor']=component('../components/code/TypeScriptEditor.tsx');
 const {ScheduleInputEditor}=component('../components/apps/ScheduleInputEditor.tsx');
 /** Retains the exact document emitted to the outer schedule form, including malformed drafts. */
 function Harness(){const [value,setValue]=useState(text);return createElement('form',null,createElement(ScheduleInputEditor,{appID:'retained-version',text:value,onChange:next=>{text=next;setValue(next);}}));}
 const root=createRoot(document.getElementById('root'));
 /** Types JSON through the actual editor's change handler. */
 async function change(value){await act(async()=>Simulate.change(document.querySelector('textarea'),{target:{value}}));}
 /** Runs editor toolbar actions without submitting the surrounding form. */
 async function click(name){const button=[...document.querySelectorAll('button')].find(el=>el.textContent===name);assert.ok(button,name);await act(async()=>Simulate.click(button));}
 try{
  await act(async()=>root.render(createElement(Harness)));
  assert.deepEqual(targets,['active-version']);assert.equal(document.querySelectorAll('textarea').length,1);
  assert.equal(document.querySelector('form').checkValidity(),false);
  await change('{"message":"hello","count":"0"}');
  assert.match(document.body.textContent,/\/count.*must be integer/);
  assert.equal(document.querySelector('form').checkValidity(),false);
  const valid='{"message":"hello","count":0,"enabled":false,"extra":{"keep":true}}';
  await change(valid);assert.equal(document.querySelector('form').checkValidity(),true);
  await click('Format');assert.deepEqual(JSON.parse(text),JSON.parse(valid));
  await click('Example');assert.deepEqual(JSON.parse(text),JSON.parse(valid));
  await click('Use this example');assert.equal(JSON.parse(text).count,0);
  await change('{broken');assert.equal(text,'{broken');assert.equal(document.querySelector('form').checkValidity(),false);
  assert.equal([...document.querySelectorAll('button')].find(el=>el.textContent==='Format').disabled,true);
  await change(valid);assert.equal(document.querySelector('form').checkValidity(),true);
 }finally{await act(async()=>root.unmount());dom.window.close();globalThis.window=previous.window;globalThis.document=previous.document;globalThis.IS_REACT_ACT_ENVIRONMENT=previous.act;}
});
