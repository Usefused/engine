import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import ts from "typescript";
import { act, createElement } from "react";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { JSDOM } from "jsdom";

/** Flat presentation must preserve exact capability and version callbacks without nesting disclosures or widening scope. */
test("flat capabilities expose direct operations, events and immutable versions", async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const before = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const require = createRequire(import.meta.url), exports = {};
  const code = ts.transpileModule(readFileSync(new URL('../components/apps/CompactServiceCapabilities.tsx', import.meta.url), 'utf8'), {compilerOptions:{module:ts.ModuleKind.CommonJS,target:ts.ScriptTarget.ES2022,jsx:ts.JsxEmit.ReactJSX}}).outputText;
  /** Keep the select native here; option identity and callback behavior are the contract under test. */
  function resolve(name) {
    // The production select enhancement is independently covered; native events still carry the exact selected ID.
    if (name === '~/components/forms/Select') return {Select: ({density, ...props}) => createElement('select', props)};
    return require(name);
  }
  new Function('require','exports',code)(resolve,exports);
  const calls = [];
  const props = { item:{service:{id:'crm',name:'CRM',endpoint_count:1,webhook_count:1,resources:[{id:'customers',name:'Customers'},{id:'invoices',name:'Invoices'}]},serviceVersions:[{id:'v1-id',name:'v1'},{id:'v2-id',name:'v2'}],integrations:[{id:'get-id',name:'getCustomer',method:'GET',path:'/customers',description:'Find customer',resource_id:'customers'}],webhooks:[{id:'event-id',name:'customer.created',description:'Created'}]},selectedOperations:new Set(['get-id']),selectedEvents:new Set(),version:'v1-id',loaded:true,loading:false,allowEvents:true,resourceLoading:{},resourceMore:{},
    // Each callback records its category so event selection cannot masquerade as an outbound operation.
    onOperation: (id) => calls.push(['operation',id]), onEvent: (id) => calls.push(['event',id]), onVersion: (id) => calls.push(['version',id]), onLoad: () => calls.push(['load']), onResource: (id,name) => calls.push(['resource',id,name]), onMore: (name) => calls.push(['more',name]) };
  const root = createRoot(document.getElementById('root'));
  try {
    await act(/** Render the real flat component with a pinned, preselected provider contract. */ async () => root.render(createElement(exports.CompactServiceCapabilities, props)));
    assert.equal(document.querySelectorAll('details,summary,[aria-expanded]').length, 0);
    assert.equal(document.querySelector('input[aria-label="getCustomer"]').checked, true);
    assert.equal(document.querySelectorAll('[role="tab"]').length, 2);
    await act(/** Toggling one visible operation must preserve every other capability category. */ async () => Simulate.change(document.querySelector('input[aria-label="getCustomer"]')));
    await act(/** A new version is an immutable ID, not a display label or latest-version request. */ async () => Simulate.change(document.querySelector('select'), {target:{value:'v2-id'}}));
    await act(/** Keyboard navigation selects Events directly without opening another disclosure. */ async () => Simulate.keyDown(document.querySelector('[role="tablist"]'), {target:document.querySelector('[role="tab"]'),key:'ArrowRight'}));
    assert.match(document.activeElement.textContent, /Events/);
    assert.equal(document.querySelector('input[aria-label="getCustomer"]'), null);
    await act(/** Provider event scope is independent of the operation selected above. */ async () => Simulate.change(document.querySelector('input[aria-label="customer.created"]')));
    assert.deepEqual(calls, [['operation','get-id'],['version','v2-id'],['event','event-id']]);
  } finally {
    await act(/** Dispose mounted effects before restoring the caller's document. */ async () => root.unmount()); dom.window.close();
    globalThis.window = before.window; globalThis.document = before.document; globalThis.IS_REACT_ACT_ENVIRONMENT = before.act;
  }
});
