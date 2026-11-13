import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync, existsSync } from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import ts from 'typescript';
import { act, createElement } from 'react';
import { JSDOM } from 'jsdom';
import * as permissions from './current-actor-permissions.ts';
import * as contracts from './app-try-contract.ts';
import * as fields from './unified-app-input-fields.ts';
import * as validation from './schedule-input-validation.ts';
import * as executionURL from './app-execution-url.ts';
const require = createRequire(import.meta.url);
const appRoot = fileURLToPath(new URL('../', import.meta.url));

/** Loads real sidebar components while replacing only the Engine network boundary. */
function loadSidebar(api, accessState = { access: { subject_id: "user", workspace_id: "workspace", grants: [{ permission: "app.api.tokens.manage", resource_type: "APP", resource_id: "family" }] }, loading: false, failed: false }) {
  const cache = new Map();
  /** Transpiles browser modules with their normal React implementation and local imports. */
  function load(filename) {
    // Reusing exports keeps React component identities stable across nested imports.
    if (cache.has(filename)) return cache.get(filename);
    const exports = {}; cache.set(filename, exports);
    const compiled = ts.transpileModule(readFileSync(filename, 'utf8'), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
    /** Maps only application aliases; external packages use Node's installed dependencies. */
    function resolve(name) {
      const mocks = { '~/lib/current-actor-access': permissions, '~/components/access/CurrentActorAccess': { useCurrentActorAccess: () => accessState }, '~/lib/api': { api }, '~/lib/app-try-contract': contracts, '~/lib/unified-app-input-fields': fields, '~/lib/schedule-input-validation': validation, '~/lib/app-execution-url': executionURL };
      // Pure helpers remain their real ESM modules, avoiding duplicate module-format state.
      if (mocks[name]) return mocks[name];
      if (!name.startsWith('.') && !name.startsWith('~/')) return require(name);
      const base = name.startsWith('~/') ? path.join(appRoot, name.slice(2)) : path.resolve(path.dirname(filename), name);
      const found = [base, `${base}.tsx`, `${base}.ts`, `${base}.js`].find(candidate => existsSync(candidate));
      assert.ok(found, name);
      // The existing branded select is a shipped CommonJS module.
      if (found.endsWith('.js')) return require(found);
      return load(found);
    }
    new Function('require', 'exports', compiled)(resolve, exports);
    return exports;
  }
  return load(path.join(appRoot, 'components/apps/AppTrySidebar.tsx'));
}

/** Builds two genuinely different operation inputs to expose stale-form regressions. */
function schemaDocument() {
  return { 'x-fused-app-id': 'version', paths: { '/v1/apps/{app_id}/executions': { post: { requestBody: { content: { 'application/json': { schema: { oneOf: [
    { properties: { operation: { const: 'create' }, selector: {}, bucket_id: { enum: ['sandbox'], 'x-fused-bucket-names': { sandbox: 'Sandbox' } }, input: { type: 'object', required: ['name'], properties: { name: { type: 'string', minLength: 1 } } } } },
    { properties: { operation: { const: 'read' }, selector: {}, input: { type: 'object', required: ['count'], properties: { count: { type: 'integer', minimum: 1 } } } } },
   ] } } } } } } } };
}

// Real input controls and runtime requests must remain bound to the selected operation and bucket.
test('sidebar loads exact forms, sends once, clears tokens, and resets on operation changes', async () => {
  const dom = new JSDOM('<button id="trigger">Try</button><div id="root"></div>', { url: 'http://localhost' });
  const prior = { MutationObserver: globalThis.MutationObserver, window: globalThis.window, document: globalThis.document, HTMLElement: globalThis.HTMLElement, fetch: globalThis.fetch, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.MutationObserver = dom.window.MutationObserver; globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.HTMLElement = dom.window.HTMLElement; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { createRoot } = await import('react-dom/client'); const { Simulate } = await import('react-dom/test-utils');
  const root = createRoot(document.getElementById('root'));
  const calls = []; const issued = []; const revoked = []; const readinessCalls = []; let complete;
  /** Captures a single pending runtime request without contacting a provider. */
  globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Promise(resolve => { complete = resolve; }); };
  const { AppTrySidebar } = loadSidebar({ appTokens: { generate: async (...args) => { issued.push(args); return { token: "automatic-test-token" }; }, revoke: async (...args) => { revoked.push(args); } }, appConfig: {
    /** Returns the exact deployed public schema. */
    openAPI: async () => schemaDocument(),
    /** Supplies credential-free readiness independently of execution. */
    tryReadiness: async (...args) => new Promise(/** Lets the test deliver stale responses after a newer selection. */ resolve => { readinessCalls.push({ args, resolve }); }),
  } });
  /** Uses actual React input events rather than editing component state directly. */
  async function change(label, value) { const element = document.querySelector(`[aria-label="${label}"]`); assert.ok(element, label); await act(async () => Simulate.change(element, { target: { value } })); }
  /** Finds the visible execution button independently of icon markup. */
  function runButton() { return [...document.querySelectorAll('button')].find(button => button.textContent === 'Run'); }
  try {
    document.getElementById('trigger').focus();
    await act(async () => root.render(createElement(AppTrySidebar, { target: { versionID: 'version', executionID: 'version', familyID: 'family', name: 'Issues', unified: false }, /** Keeps this directly mounted sidebar open until test cleanup. */ onClose() {} })));
    assert.ok(document.querySelector('[role="dialog"]'));
    assert.equal(document.body.style.overflow, 'hidden');
    assert.equal(runButton().disabled, true);
    // Let the first debounced metadata request start before changing its identity.
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 280)); });
    assert.equal(readinessCalls.length, 1);
    await change('name', 'Alice'); await change('Bucket', 'sandbox'); await change('Connected user', 'alice');
    // A response for configured routing must never appear under the new selected bucket.
    await act(async () => { await new Promise(resolve => setTimeout(resolve, 280)); });
    assert.equal(readinessCalls.length, 2);
    assert.equal(readinessCalls[0].args[4].aborted, true);
    assert.equal(readinessCalls[1].args[2], 'sandbox');
    assert.equal(readinessCalls[1].args[3], 'alice');
    await act(async () => readinessCalls[0].resolve({ requirements: [], runtime_buckets: [], note: 'Stale readiness' }));
    assert.doesNotMatch(document.body.textContent, /Stale readiness/);
    assert.match(document.body.textContent, /Checking this operation/);
    await act(async () => readinessCalls[1].resolve({ requirements: [], runtime_buckets: [], note: 'Current readiness' }));
    assert.match(document.body.textContent, /Current readiness/);
    await act(async () => { Simulate.click(runButton()); Simulate.click(runButton()); });
    assert.equal(calls.length, 1);
    assert.equal(issued.length, 1);
    assert.equal(issued[0][0], 'family');
    assert.deepEqual(issued[0][1].allow, ['create']);
    assert.equal(issued[0][1].expires_in, 300);
    assert.equal(calls[0].options.headers.Authorization, 'Bearer automatic-test-token');
    assert.equal(calls[0].options.credentials, 'omit');
    assert.equal(calls[0].options.redirect, 'error');
    assert.deepEqual(JSON.parse(calls[0].options.body), { operation: 'create', input: { name: 'Alice' }, bucket_id: 'sandbox', selector: { end_user_ref: 'alice' } });
    await act(async () => complete({ status: 409, ok: false, text: async () => '{"error":{"code":"connection_required"}}' }));
    assert.equal(document.querySelector('[aria-label="Execution token"]'), null);
    assert.deepEqual(revoked, [['family', issued[0][1].name]]);
    assert.match(document.querySelector('[aria-label="Execution response"]').textContent, /HTTP 409/);
    await change('Operation', 'read');
    assert.equal(document.querySelector('[aria-label="name"]'), null);
    assert.ok(document.querySelector('[aria-label="count"]'));
    assert.equal(document.querySelector('[aria-label="Bucket"]').disabled, true);
    assert.equal(document.querySelector('[aria-label="Execution response"]'), null);
    await change('count', '0'); assert.equal(runButton().disabled, true);
    await change('count', '1'); assert.equal(runButton().disabled, false);
    const checkbox = document.querySelector('input[type="checkbox"]');
    await act(async () => Simulate.change(checkbox, { target: { checked: true } }));
    assert.equal(runButton().disabled, true);
    await change('Execution token', 'synthetic-test-token');
    await act(async () => Simulate.click(runButton()));
    assert.equal(issued.length, 1);
    assert.equal(calls[1].options.headers.Authorization, 'Bearer synthetic-test-token');
    await act(async () => complete({ status: 200, ok: true, text: async () => '{}' }));
    assert.equal(document.querySelector('[aria-label="Execution token"]').value, '');
    assert.equal(revoked.length, 1);
    // A settled asynchronous response no longer owns the in-flight ref, but its token still belongs to the sidebar.
    await act(async () => Simulate.change(checkbox, { target: { checked: false } }));
    await act(async () => Simulate.click(runButton()));
    await act(async () => complete({ status: 202, ok: true, text: async () => '{}' }));
    assert.equal(issued.length, 2);
    assert.equal(revoked.length, 1);

  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.MutationObserver = prior.MutationObserver; globalThis.window = prior.window; globalThis.document = prior.document; globalThis.HTMLElement = prior.HTMLElement; globalThis.fetch = prior.fetch; globalThis.IS_REACT_ACT_ENVIRONMENT = prior.act;
  }
  assert.deepEqual(revoked[1], ['family', issued[1][1].name], 'unmount revokes the retained asynchronous token');
});

// Read access and authority on another family must not enable automatic credential issuance.
test('automatic mode respects exact app permissions and offers supplied-token mode', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const keys = ['window', 'document', 'HTMLElement', 'MutationObserver', 'IS_REACT_ACT_ENVIRONMENT'];
  const prior = Object.fromEntries(keys.map(key => [key, globalThis[key]]));
  Object.assign(globalThis, { window: dom.window, document: dom.window.document, HTMLElement: dom.window.HTMLElement, MutationObserver: dom.window.MutationObserver, IS_REACT_ACT_ENVIRONMENT: true });
  const { createRoot } = await import('react-dom/client'); const { Simulate } = await import('react-dom/test-utils');
  const root = createRoot(document.getElementById('root'));
  const state = { access: { workspace_id: 'workspace', grants: [{ permission: 'app.api.tokens.manage', resource_type: 'APP', resource_id: 'another-family' }, { permission: 'app.api.read', resource_type: 'APP', resource_id: 'family' }] }, loading: false, failed: false };
  let issuances = 0;
  const { AppTrySidebar } = loadSidebar({ appConfig: { openAPI: async () => schemaDocument(), tryReadiness: async () => ({ requirements: [] }) }, appTokens: { generate: async () => { issuances++; return { token: 'unexpected' }; } } }, state);
  const props = { target: { versionID: 'version', executionID: 'version', familyID: 'family', name: 'Issues', unified: false }, onClose() {} };
  const button = () => [...document.querySelectorAll('button')].find(item => item.textContent === 'Run');
  try {
    await act(async () => root.render(createElement(AppTrySidebar, props)));
    await act(async () => Simulate.change(document.querySelector('[aria-label="name"]'), { target: { value: 'Alice' } }));
    assert.equal(button().disabled, true);
    assert.match(document.body.textContent, /need permission to manage execution tokens/);
    assert.equal(issuances, 0);
    await act(async () => Simulate.change(document.querySelector('input[type="checkbox"]'), { target: { checked: true } }));
    await act(async () => Simulate.change(document.querySelector('[aria-label="Execution token"]'), { target: { value: 'provided' } }));
    assert.equal(button().disabled, false);
    await act(async () => Simulate.change(document.querySelector('input[type="checkbox"]'), { target: { checked: false } }));
    assert.equal(button().disabled, true);
    for (const next of [{ loading: true, failed: false }, { loading: false, failed: true }]) {
      Object.assign(state, next);
      await act(async () => root.render(createElement(AppTrySidebar, { ...props })));
      assert.equal(button().disabled, true);
    }
    assert.equal(issuances, 0);
  } finally {
    await act(async () => root.unmount()); dom.window.close(); Object.assign(globalThis, prior);
  }
});
