import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { JSDOM } from 'jsdom';
import { renderToStaticMarkup } from 'react-dom/server';
import { hasResourcePermission } from './current-actor-permissions.ts';

const require = createRequire(import.meta.url);
let access = null;
let calls = 0;
let listCalls = 0;
/** Loads production components with only the access snapshot and issuance transport replaced. */
function load(path, dependencies = {}) {
  const source = readFileSync(new URL(path, import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function('require', 'exports', compiled)(name => dependencies[name] ?? require(name), exports);
  return exports;
}
const { AppExecutionTokens, customTokenExpirySeconds } = load('../components/apps/AppExecutionTokens.tsx', {
  '~/components/agent/FusedAgentContext': { useFusedAgent: () => null },
  './AppExecutionTokenList': load('../components/apps/AppExecutionTokenList.tsx', {
    // Metadata must only load after the permission-checked panel opens.
    '~/lib/api': { api: { mcpGraphql: async () => { listCalls++; return { appTokens: [] }; } } },
    '~/components/Toast': { useToast: () => ({}) },
    '~/components/forms/Select': load('../components/forms/Select.ts'),
  }),
  '~/lib/api': { api: { appTokens: { generate: async () => { calls++; throw new Error('Unexpected issuance'); } } } },
  '~/lib/current-actor-access': { hasResourcePermission },
  '~/components/access/CurrentActorAccess': { useCurrentActorAccess: () => ({ access, loading: false, failed: false }) },
  '~/components/CopyValue': load('../components/CopyValue.tsx'),
  '~/components/forms/FieldLabel': load('../components/forms/FieldLabel.ts'),
  '~/components/forms/Select': load('../components/forms/Select.ts'),
});

// App managers, sibling-family grants, and another app type's token grants must never unlock issuance.
test('execution token controls require the exact typed family permission', () => {
  for (const kind of ['sdk', 'mcp', 'api', 'unified_app']) {
    for (const grant of [null,
      { permission: `app.${kind}.manage`, resource_type: 'APP', resource_id: 'family' },
      { permission: `app.${kind}.tokens.manage`, resource_type: 'APP', resource_id: 'sibling' },
      { permission: 'app.webhook.tokens.manage', resource_type: 'APP', resource_id: 'family' },
      { permission: `app.${kind}.tokens.manage`, resource_type: 'WORKSPACE', resource_id: 'other-workspace' },
    ]) {
      access = { subject_id: 'reader', workspace_id: 'workspace', grants: grant ? [grant] : [] };
      const html = renderToStaticMarkup(createElement(AppExecutionTokens, { familyID: 'family', kind }));
      assert.doesNotMatch(html, /<button/);
      assert.match(html, /need permission/);
    }
    for (const resource_type of ['APP', 'WORKSPACE']) {
      access = { subject_id: 'manager', workspace_id: 'workspace', grants: [{ permission: `app.${kind}.tokens.manage`, resource_type, resource_id: resource_type === 'APP' ? 'family' : 'workspace' }] };
      const html = renderToStaticMarkup(createElement(AppExecutionTokens, { familyID: 'family', kind }));
      assert.match(html, /Execution tokens/);
      assert.match(html, /aria-haspopup="dialog"/);
      assert.doesNotMatch(html, /<form/);
    }
  }
  assert.equal(calls, 0, 'rendering any detail page must never issue a token');
});

// The overview must not prefetch tokens or leave an off-screen panel mounted after dismissal.
test('execution tokens open in a lazy side panel and restore trigger focus on close', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  access = { subject_id: 'manager', workspace_id: 'workspace', grants: [{ permission: 'app.mcp.tokens.manage', resource_type: 'APP', resource_id: 'family' }] };
  listCalls = 0;
  const root = createRoot(document.getElementById('root'));
  try {
    await act(async () => root.render(createElement(AppExecutionTokens, { familyID: 'family', kind: 'mcp' })));
    assert.equal(listCalls, 0); assert.equal(document.querySelector('[role="dialog"]'), null);
    const trigger = document.querySelector('button');
    await act(async () => trigger.click());
    assert.equal(listCalls, 1); assert.ok(document.querySelector('[data-fused-detail-sidebar]'));
    assert.equal(document.activeElement.textContent, 'Execution tokens');
    const generate = [...document.querySelectorAll('[role="dialog"] header button')].find(button => button.textContent === 'Generate token');
    assert.ok(generate, 'generation uses the shared panel header');
    assert.equal(calls, 0, 'opening the panel never issues a credential');
    await act(async () => document.querySelector('[aria-label="Close execution tokens"]').click());
    assert.equal(document.querySelector('[role="dialog"]'), null); assert.equal(document.activeElement, trigger);
    await act(async () => trigger.click());
    assert.equal(listCalls, 2);
    // Losing the exact permission destroys the open panel as well as its launcher.
    access = { ...access, grants: [] };
    await act(async () => root.render(createElement(AppExecutionTokens, { familyID: 'family', kind: 'mcp' })));
    assert.equal(document.querySelector('[role="dialog"]'), null);
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});

// Exercise the real list, confirmation, failure, and refresh transitions with credential-free metadata.
test('token history requires confirmation, retains failures, and refreshes after revocation', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let rows = [
    { id: 'active', name: 'backend & production', status: 'active', allow: ['*'], created_at: '2026-10-01T10:00:00Z', expires_at: null, last_used_at: null },
    { id: 'expired', name: 'old-client', status: 'expired', allow: ['Stripe.PostCustomers'], created_at: '2026-09-01T10:00:00Z', expires_at: '2026-09-02T10:00:00Z', last_used_at: null },
  ];
  let deleteCount = 0;
  let rejectRevoke = true;
  let revokedName = '';
  let failList = false;
  let confirmResult = false;
  let confirmations = 0;
  const messages = [];
  const { AppExecutionTokenList } = load('../components/apps/AppExecutionTokenList.tsx', {
    '~/components/Toast': { useToast: () => ({
      // The shared prompt identifies the exact token and offers an explicit revoke action.
      confirm: async (message, options) => { confirmations++; assert.match(message, /backend & production/); assert.equal(options.confirmLabel, 'Revoke token'); return confirmResult; },
      // Capture feedback while retaining the component's real success and failure decisions.
      success: message => messages.push(message),
      error: message => messages.push(message),
    }) },
    '~/components/forms/Select': load('../components/forms/Select.ts'),
    '~/lib/api': { api: {
      // A fresh clone makes post-mutation refresh behavior observable, rather than sharing fixture object state.
      mcpGraphql: async (_query, variables) => {
        assert.equal(variables.familyID, 'family');
        // A rejected list must not be rendered as an authoritative empty result.
        if (failList) throw new Error('Metadata unavailable');
        return { appTokens: structuredClone(rows) };
      },
      appTokens: {
        // Engine revocation is exact-family and exact-name, including URL-significant characters.
        revoke: async (family, name) => {
          deleteCount++; assert.equal(family, 'family'); assert.equal(name, 'backend & production');
          // The first failure proves confirmation remains actionable without an automatic retry.
          if (rejectRevoke) throw new Error('Temporary revocation failure');
          rows = rows.map(row => ({ ...row, status: 'revoked' }));
        },
      },
    } },
  });
  const container = document.getElementById('root');
  const root = createRoot(container);
  /** Dispatches a user click and flushes the resulting React effects. */
  async function click(label) {
    const button = [...container.querySelectorAll('button')].find(item => (item.getAttribute('aria-label') || item.textContent) === label);
    assert.ok(button, `Missing button ${label}`);
    await act(async () => button.click());
  }
  try {
    // Mounting loads metadata only; no token may be revoked merely by viewing the page.
    await act(async () => root.render(createElement(AppExecutionTokenList, { familyID: 'family', revision: 0, onRevoked: name => { revokedName = name; } })));
    assert.match(container.textContent, /backend & production/); assert.doesNotMatch(container.textContent, /old-client/);
    assert.equal(deleteCount, 0);
    await click('Revoke backend & production'); assert.equal(deleteCount, 0); assert.equal(confirmations, 1);
    confirmResult = true; await click('Revoke backend & production');
    assert.equal(deleteCount, 1); assert.match(messages.at(-1), /Temporary revocation failure/);
    assert.equal(revokedName, '');
    rejectRevoke = false; await click('Revoke backend & production');
    assert.equal(deleteCount, 2); assert.equal(revokedName, 'backend & production');
    assert.match(container.textContent, /No active execution tokens/);
    assert.match(messages.at(-1), /revoked/); assert.equal(confirmations, 3);
    // History remains visible but never offers another revoke for a terminated credential.
    await act(async () => { const select = container.querySelector('select'); select.value = 'all'; select.dispatchEvent(new dom.window.Event('change', { bubbles: true })); });
    assert.match(container.textContent, /old-client/);
    assert.equal(container.querySelectorAll('button[aria-label^="Revoke "]').length, 0);
    failList = true; await click('Refresh execution tokens');
    assert.match(container.querySelector('[role="alert"]').textContent, /Metadata unavailable/);
    assert.match(container.textContent, /old-client/);
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});

// Conversion rejects inputs that could silently produce unlimited, rounded, or overflowing credentials.
test('custom token durations preserve exact positive units and Engine limits', () => {
  assert.equal(customTokenExpirySeconds('45', '60'), 2700);
  assert.equal(customTokenExpirySeconds('6', '3600'), 21600);
  assert.equal(customTokenExpirySeconds('14', '86400'), 1209600);
  assert.equal(customTokenExpirySeconds('106751', '86400'), 9223286400);
  for (const amount of ['', ' ', '0', '-1', '1.5', 'Infinity', 'NaN', '106752']) {
    assert.equal(customTokenExpirySeconds(amount, '86400'), null);
  }
  assert.equal(customTokenExpirySeconds('1', 'unknown'), null);
});
