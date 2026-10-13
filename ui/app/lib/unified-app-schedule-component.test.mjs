import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { Simulate } from 'react-dom/test-utils';
import { JSDOM } from 'jsdom';
import * as helpers from './unified-app-schedule.ts';
const require = createRequire(import.meta.url);

// Actual form events exercise polling races and draft retention beyond pure helper tests.
test('schedule forms preserve errors, precise dates, and mutation results across polling', async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = {window:globalThis.window,document:globalThis.document,act:globalThis.IS_REACT_ACT_ENVIRONMENT,interval:globalThis.setInterval,clear:globalThis.clearInterval};
  globalThis.window=dom.window; globalThis.document=dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT=true;
  let poll, pending, hold=false, failure=false;
  let items=[{id:'one',name:'Precise run',input:{recipient:'fixture'},cron:'',timezone:'UTC',run_at:'2026-12-01T09:00:45.123Z',enabled:true,revision:1,next_run_at:'2026-12-01T09:00:45.123Z',last_status:'',last_app_id:null,last_execution_id:null,last_started_at:null}];
  const saved=[];
  // Trigger production polling deterministically without wall-clock sleeps.
  globalThis.setInterval=callback=>{poll=callback;return 1;};
  globalThis.clearInterval=()=>{};
  const dependencies={
    '~/lib/unified-app-schedule':helpers,
    // The surrounding schedule test keeps JSON events focused; schema fields have their own component coverage.
    './ScheduleInputEditor': { ScheduleInputEditor: ({text,onChange}) => createElement('textarea',{value:text,onChange:event=>onChange(event.target.value)}) },
    '@remix-run/react':{Link:'a'},
    // Surrounding notifications are inert except for deliberate confirmation of disposable fixture deletion.
    '~/components/Toast':{useToast:()=>({success:()=>{},confirm:async()=>true})},
    '~/lib/api':{api:{schedules:{
      // A copied deferred response recreates an old poll arriving after the save response.
      list:async()=>{
        const schedules=structuredClone(items);
        // Only the deliberately held read is deferred; mutation refreshes proceed normally.
        if(hold)return new Promise(resolve=>{pending=()=>resolve({schedules});});
        return {schedules};
      },
      // Conflicts must leave the draft intact; successful writes advance its reviewed revision.
      save:async(_app,item)=>{
        // A simulated concurrent edit rejects without changing fixture state.
        if(failure)throw new Error('schedule changed; reload before retrying');
        saved.push(item);items=[{...items[0],...item,revision:item.revision+1}];return {id:item.id,saved:true};
      },
      // Delete affects this test's private fixture only.
      remove:async()=>{items=[];return {deleted:true};},
    }}},
  };
  const source=readFileSync(new URL('../components/apps/UnifiedAppSchedules.tsx',import.meta.url),'utf8');
  const compiled=ts.transpileModule(source,{compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
  const exports={};
  // Replace remote dependencies while executing the real component and hooks.
  new Function('require','exports',compiled)(name=>dependencies[name]??require(name),exports);
  const root=createRoot(document.getElementById('root'));
  // Visible button text avoids coupling the regression to element order.
  async function click(label){
    const target=[...document.querySelectorAll('button')].find(button=>button.textContent===label);
    assert.ok(target,`missing ${label}`);await act(async()=>Simulate.click(target));
  }
  // Submit the production form handler after the controlled editor has updated.
  async function submit(){await act(async()=>Simulate.submit(document.querySelector('form')));}
  // Controlled JSON edits preserve the same event contract as browser typing.
  async function input(value){await act(async()=>Simulate.change(document.querySelector('textarea'),{target:{value}}));}
  try{
    await act(async()=>root.render(createElement(exports.UnifiedAppSchedules,{appID:'fixture'})));
    await click('Edit');await input('{broken');await submit();
    assert.match(document.querySelector('[role="alert"]').textContent,/valid JSON/);
    await act(async()=>poll());
    assert.match(document.querySelector('[role="alert"]').textContent,/valid JSON/);
    assert.equal(document.querySelector('textarea').value,'{broken');
    await input('{"recipient":"revised"}');
    hold=true;await act(async()=>{void poll();});hold=false;await submit();
    assert.equal(saved.at(-1).run_at,'2026-12-01T09:00:45.123Z');
    assert.equal(saved.at(-1).input.recipient,'revised');
    await act(async()=>pending());await click('Edit');
    assert.match(document.querySelector('textarea').value,/revised/,'late polls cannot replace saved input');
    failure=true;await submit();await act(async()=>poll());
    assert.match(document.querySelector('[role="alert"]').textContent,/schedule changed/);
    assert.ok(document.querySelector('form'));
    failure=false;await click('Cancel');await click('Pause');assert.equal(saved.at(-1).enabled,false);
    await click('Resume');assert.equal(saved.at(-1).enabled,true);
    items=[{...items[0],next_run_at:null,last_status:'running'}];await act(async()=>poll());
    assert.match(document.querySelector('article').textContent,/Running/);
    assert.doesNotMatch(document.querySelector('article').textContent,/Completed/);
    assert.equal([...document.querySelectorAll('button')].find(button=>button.textContent==='Edit').disabled,true);
    items=[{...items[0],last_status:'succeeded'}];await act(async()=>poll());
    assert.match(document.querySelector('article').textContent,/Completed/);
    await click('Delete');assert.match(document.body.textContent,/No scheduled runs yet/);
  }finally{
    await act(async()=>root.unmount());dom.window.close();
    globalThis.window=previous.window;globalThis.document=previous.document;globalThis.IS_REACT_ACT_ENVIRONMENT=previous.act;
    globalThis.setInterval=previous.interval;globalThis.clearInterval=previous.clear;
  }
});
