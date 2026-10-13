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

// Real component rendering verifies active settings, edit links, failure recovery and late-response isolation.
test('webhook panel renders saved events and registration management without changing deployment', async () => {
  const dom = new JSDOM('<div id="root"></div>');
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let fail = false, pending;
  const calls = [];
  const source = { config: { version: '1.2.0', webhook_attachment: 'billing events', services: { Stripe: { version: 'v1', webhooks: ['invoice.paid'] } } } };
  const require = createRequire(import.meta.url);
  const dependencies = {
    // A native anchor preserves observable link destinations without requiring a router fixture.
    '@remix-run/react': { Link: ({ to, ...props }) => createElement('a', { ...props, href: to }) },
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
  const code = readFileSync(new URL('../components/apps/UnifiedAppWebhooks.tsx', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(code, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  // Execute production hooks and markup with transport dependencies isolated to this fixture.
  new Function('require', 'exports', compiled)(name => dependencies[name] ?? require(name), exports);
  const root = createRoot(document.getElementById('root'));
  /** Render an app change through production effect cleanup and source loading. */
  async function render(appID, active = true) { await act(async () => root.render(createElement(exports.UnifiedAppWebhooks, { appID, active }))); }
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
    assert.match(document.querySelector('a').textContent, /Add webhook trigger/);
    assert.doesNotMatch(document.body.textContent, /invoice.paid/);
    assert.deepEqual(calls, ['active', 'retained', 'failure', 'failure', 'slow', 'empty']);
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
