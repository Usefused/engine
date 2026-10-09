import assert from 'node:assert/strict';
import test from 'node:test';
import { createRequire } from 'node:module';
import { build } from 'esbuild';
import React, { act } from 'react';
import { createRoot } from 'react-dom/client';
import { JSDOM } from 'jsdom';

const compiled = await build({entryPoints:[new URL('../components/agent/FusedAgentLauncher.tsx',import.meta.url).pathname],bundle:true,write:false,platform:'node',format:'cjs',jsx:'automatic',external:['react','react/jsx-runtime']});
const module = {exports:{}};
new Function('require','module','exports',compiled.outputFiles[0].text)(createRequire(import.meta.url),module,module.exports);
const Launcher = module.exports.default;

// Exercise real React pointer handlers; layout alone cannot verify suppression of a drag's trailing click.
test('dragging, ordinary clicks, chat toggles and resizing preserve launcher behavior', async () => {
 const dom = new JSDOM('<div id="root"></div>');
 const previous = {window:globalThis.window,document:globalThis.document,act:globalThis.IS_REACT_ACT_ENVIRONMENT};
 globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
 const root = createRoot(document.getElementById('root'));
 let opened = 0;
 /** Count opens instead of invoking an agent or model. */
 function open() { opened++; }
 /** Toggle the mounted launcher exactly as the production provider does. */
 async function render(visible) { await act(async()=>root.render(React.createElement(Launcher,{visible,onOpen:open}))); }
 /** Dispatch the browser's pointer shape, including the primary-pointer identity. */
 async function pointer(type,x,y) {
  const button=document.querySelector('button');
  button.setPointerCapture=()=>{};
  button.getBoundingClientRect=()=>({left:600,top:650,width:48,height:48});
  const event=new dom.window.MouseEvent(type,{bubbles:true,clientX:x,clientY:y,button:0});
  Object.defineProperties(event,{pointerId:{value:1},isPrimary:{value:true}});
  await act(async()=>button.dispatchEvent(event));
 }
 /** Simulate the click emitted after pointer-up or keyboard activation. */
 async function click(detail) { await act(async()=>document.querySelector('button').dispatchEvent(new dom.window.MouseEvent('click',{bubbles:true,detail}))); }
 try {
  await render(true);
  await pointer('pointerdown',624,674); await pointer('pointermove',400,200); await pointer('pointerup',400,200); await click(1);
  assert.equal(opened,0,'drag must not open chat');
  assert.equal(document.querySelector('button').style.left,'376px');
  assert.equal(document.querySelector('button').style.top,'176px');
  await render(false); assert.equal(document.querySelector('button'),null);
  await render(true); assert.equal(document.querySelector('button').style.left,'376px');
  Object.defineProperty(window,'innerWidth',{value:320,configurable:true});
  await act(async()=>window.dispatchEvent(new dom.window.Event('resize')));
  assert.equal(document.querySelector('button').style.left,'260px','resize keeps the whole button reachable');
  await pointer('pointerdown',624,674); await pointer('pointermove',626,675); await pointer('pointerup',626,675); await click(1);
  assert.equal(opened,1,'small hand jitter remains a normal click');
  await click(0); assert.equal(opened,2,'keyboard activation stays available');
 } finally {
  await act(async()=>root.unmount()); dom.window.close();
  globalThis.window=previous.window; globalThis.document=previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT=previous.act;
 }
});
