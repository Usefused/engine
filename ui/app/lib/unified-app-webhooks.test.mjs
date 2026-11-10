import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { Simulate } from 'react-dom/test-utils';
import { JSDOM } from 'jsdom';
import { unifiedAppTabs, selectedUnifiedAppTab } from './unified-app-tabs.ts';

// Automation follows Overview, with denied deep links unable to mount private source readers.
test('trigger tabs precede activity and retain management permission boundaries', () => {
  assert.deepEqual(unifiedAppTabs(true, true).map(tab => tab.value), ['overview', 'webhooks', 'schedules', 'analytics', 'requests', 'changes']);
  assert.deepEqual(unifiedAppTabs(true, false).map(tab => tab.value), ['overview', 'webhooks', 'schedules', 'changes']);
  for (const activity of [true, false]) {
    const tabs = unifiedAppTabs(false, activity);
    assert.equal(selectedUnifiedAppTab(tabs, 'webhooks'), 'overview');
    assert.equal(selectedUnifiedAppTab(tabs, 'schedules'), 'overview');
    assert.equal(selectedUnifiedAppTab(tabs, 'unknown'), 'overview');
  }
});

// Real components verify saved settings, existing registration discovery, recovery, and late-response isolation.
test('webhook panel renders saved events and registration management without changing deployment', async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let fail = false, pending;
  const calls = [];
  const listingCalls = [], navigations = [];
  let listingFailure = false, lateListing;
  const source = { config: { version: '1.2.0', webhook_attachment: 'billing events', services: { Stripe: { version: 'v1', webhooks: ['invoice.paid'] } } } };
  const require = createRequire(import.meta.url);
  const dependencies = {
    // A native anchor preserves observable link destinations without requiring a router fixture.
    '@remix-run/react': {
      // Native anchors expose destinations while navigation remains observable without a router fixture.
      Link: ({ to, ...props }) => createElement('a', { ...props, href: to }),
      // Registration selection may open an editor but must not write configuration.
      useNavigate: () => (url) => navigations.push(url),
    },
    '~/lib/webhook-discovery-api': {
      // Persisted registration labels intentionally differ from receiving slugs to catch wrong attachment identities.
      webhookListings: async (filters, signal) => {
        listingCalls.push({ ...filters, signal });
        // Failed and delayed searches exercise the same recovery and cancellation paths as real transport.
        if (listingFailure) throw new Error('Registration read denied');
        if (filters.search === 'slow') return new Promise(resolve => { lateListing = resolve; });
        if (filters.search === 'missing') return { items: [], total: 0 };
        return { items: [{ label: 'billing & events', slug: 'receiving-route', service_id: 'stripe', service_name: 'Stripe' }], total: 21 };
      },
    },
    '~/lib/api': { api: { appConfig: {
      // Only source reads are allowed; absence of a write mock makes accidental mutations fail this test.
      source: async id => {
        calls.push(id);
        // Simulate bounded transport failures and a departed tab's outstanding request independently.
        if (fail) throw new Error('Source unavailable');
        if (id === 'slow') return new Promise(resolve => { pending = resolve; });
        if (id === 'empty') return { config: { version: '2.0.0', services: {} } };
        return structuredClone(source);
      },
    } } },
  };
  /** Executes production hooks and markup while replacing only transport and router dependencies. */
  function loadComponent(file) {
    const code = readFileSync(new URL(`../components/apps/${file}.tsx`, import.meta.url), 'utf8');
    const compiled = ts.transpileModule(code, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
    const exports = {};
    /** Resolves the registration picker to its real implementation so the Add interaction is covered end to end. */
    function resolve(name) {
      // The local picker shares the isolated network and router fixtures with its parent.
      if (name === './WebhookRegistrationPicker') return loadComponent('WebhookRegistrationPicker');
      return dependencies[name] ?? require(name);
    }
    new Function('require', 'exports', compiled)(resolve, exports);
    return exports;
  }
  const exports = loadComponent('UnifiedAppWebhooks');
  const root = createRoot(document.getElementById('root'));
  /** Render an app change through production effect cleanup and source loading. */
  async function render(appID, active = true) { await act(async () => root.render(createElement(exports.UnifiedAppWebhooks, { appID, active }))); }
  /** Uses the controlled search event without depending on browser input tracking internals. */
  async function search(value) { await act(async () => Simulate.change(document.querySelector('input'), { target: { value } })); }
  /** Exercises named actions without coupling the test to incidental button order. */
  async function click(label) { await act(async () => Simulate.click([...document.querySelectorAll('button')].find(button => button.textContent === label))); }
  try {
    await render('active');
    assert.match(document.body.textContent, /invoice.paid/);
    assert.match(document.body.textContent, /Version 1.2.0/);
    assert.equal(document.querySelector('a').getAttribute('href'), '/integrations/unified-apps/new?edit=active#app-service-selection');
    assert.equal([...document.querySelectorAll('a')].at(-1).getAttribute('href'), '/integrations/webhooks?search=billing%20events');
    await render('retained', false);
    assert.match(document.body.textContent, /No version is receiving traffic/);
    fail = true; await render('failure');
    assert.match(document.querySelector('[role="alert"]').textContent, /Source unavailable/);
    fail = false; await act(async () => Simulate.click(document.querySelector('button')));
    assert.match(document.body.textContent, /invoice.paid/);
    await render('slow');
    assert.match(document.querySelector('[role="status"]').textContent, /Loading webhook/);
    await render('empty');
    await act(async () => pending(source));
    assert.match(document.body.textContent, /No webhook triggers yet/);
    assert.match(document.querySelector('button').textContent, /Add webhook trigger/);
    assert.doesNotMatch(document.body.textContent, /invoice.paid/);
    assert.equal(listingCalls.length, 0, 'opening the app does not fetch unrelated registrations');
    await click('Add webhook trigger');
    assert.equal(navigations.length, 0, 'Add stays on the app while registrations are listed');
    assert.match(document.body.textContent, /billing & events/);
    await click('Next');
    assert.equal(listingCalls.at(-1).offset, 20);
    await search('billing');
    assert.equal(listingCalls.at(-1).offset, 0, 'search starts from the first registration page');
    await click('Use webhook');
    assert.deepEqual(navigations, ['/integrations/unified-apps/new?edit=empty&webhook=billing%20%26%20events#app-service-selection']);
    listingFailure = true; await search('denied');
    assert.match(document.querySelector('[role="alert"]').textContent, /Registration read denied/);
    assert.doesNotMatch(document.body.textContent, /No matching webhooks/);
    listingFailure = false; await click('Try again');
    assert.match(document.body.textContent, /billing & events/);
    await search('slow');
    const staleSignal = listingCalls.at(-1).signal;
    await search('missing');
    assert.equal(staleSignal.aborted, true);
    await act(async () => lateListing({ items: [{ label: 'stale registration', service_id: 'old', slug: 'old' }], total: 1 }));
    assert.match(document.body.textContent, /No matching webhooks/);
    assert.doesNotMatch(document.body.textContent, /stale registration/);
    await render('another');
    assert.equal(document.querySelector('[aria-label="Choose an existing webhook"]'), null, 'navigation closes the previous app’s chooser');
    assert.deepEqual(calls, ['active', 'retained', 'failure', 'failure', 'slow', 'empty', 'another']);
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
