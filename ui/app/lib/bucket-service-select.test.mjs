import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement, useState } from 'react';
import { JSDOM } from 'jsdom';

const require = createRequire(import.meta.url);
/** Runs the actual selector and field label without depending on a browser bundler. */
function load(path) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  /** Resolves the one application alias while preserving production React and icon imports. */
  function resolve(name) {
    // The test runner does not have Vite's application alias mapping.
    if (name === '~/components/forms/FieldLabel') return load('../components/forms/FieldLabel.ts');
    return require(name);
  }
  new Function('require', 'exports', compiled)(resolve, exports);
  return exports;
}
const { BucketServiceSelect } = load('../components/buckets/BucketServiceSelect.tsx');

/** Clicks real controls to cover the missing-callback regression and the existing explicit-selection mode. */
test('service choices apply, show their label, and reset in both filter modes', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { createRoot } = await import('react-dom/client');
  const container = document.getElementById('root');
  const root = createRoot(container);
  let currentSearch = '';
  let currentSelection = '';
  /** Mirrors overview search-only wiring and connected-user explicit ID wiring. */
  function Fixture({ explicit }) {
    const [search, setSearch] = useState('');
    const [selected, setSelected] = useState('');
    currentSearch = search;
    currentSelection = selected;
    return createElement(BucketServiceSelect, {
      id: 'services', label: 'Service', placeholder: 'All services', allowAll: true,
      options: [{ id: 'gmail-id', label: 'Gmail' }, { id: 'calendar-id', label: 'Google Calendar' }, { id: 'orphan-id', label: 'Service unavailable' }],
      search, onSearchChange: setSearch,
      // Only explicit selectors receive a separate selected-ID handler in production.
      selectedServiceId: explicit ? selected : undefined,
      onSelectedServiceChange: explicit ? setSelected : undefined,
    });
  }
  /** Dispatches actual button clicks and waits for controlled state and effects to settle. */
  async function click(label) {
    const button = [...container.querySelectorAll('button')].find((item) => {
      // Prefer accessible names for the selector trigger, whose visible label changes.
      return (item.getAttribute('aria-label') || item.textContent) === label;
    });
    assert.ok(button, `Missing button ${label}`);
    await act(async () => { /* Flush all state changes caused by this user interaction. */ button.click(); });
  }
  try {
    for (const explicit of [false, true]) {
      await act(async () => { /* A keyed fixture gives each wiring mode fresh controlled state. */ root.render(createElement(Fixture, { key: String(explicit), explicit })); });
      await click('Service');
      await click('Gmail');
      assert.equal(container.querySelector('[aria-label="Service"]').textContent, 'Gmail');
      // Search-only filters must receive the exact ID; explicit selectors keep the query empty.
      assert.equal(currentSearch, explicit ? '' : 'gmail-id');
      assert.equal(currentSelection, explicit ? 'gmail-id' : '');
      assert.equal(container.querySelector('[aria-label="Service"]').getAttribute('aria-expanded'), 'false');
      await click('Service');
      assert.equal(container.querySelector('input').value, '');
      await click('Google Calendar');
      assert.equal(container.querySelector('[aria-label="Service"]').textContent, 'Google Calendar');
      await click('Service');
      await click('Service unavailable');
      // Unresolved metadata must not turn a selection into a search for the fallback label.
      assert.equal(currentSearch, explicit ? '' : 'orphan-id');
      await click('Service');
      await click('All services');
      assert.equal(currentSearch, '');
      assert.equal(currentSelection, '');
      assert.equal(container.querySelector('[aria-label="Service"]').textContent, 'All services');
    }
  } finally {
    await act(async () => { /* Release document listeners before restoring the host globals. */ root.unmount(); });
    dom.window.close();
    globalThis.window = previous.window;
    globalThis.document = previous.document;
    globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
