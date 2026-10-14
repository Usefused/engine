import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import ts from 'typescript';
import { act, createElement } from 'react';
import { createRoot } from 'react-dom/client';
import { renderToStaticMarkup } from 'react-dom/server';
import { JSDOM } from 'jsdom';

const require = createRequire(import.meta.url);
/** Load the actual dialog with only network IO substituted; React behavior stays real. */
function loadSecretAccess(workspace) {
  const source = readFileSync(new URL('../components/buckets/BucketSecretAccess.tsx', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function('require', 'exports', compiled)(/** Replace only the credential transport. */ name => name === '~/lib/api' ? { api: { workspace } } : require(name), exports);
  return exports.BucketSecretAccess;
}
const secret = { id: 'secret-id', bucket_id: 'bucket-id', key_name: 'test-key', credential_type: 'api_key', exportable: true };

// Merely rendering metadata must never disclose a secret or advertise access without a grant.
test('secret access controls enforce supported types and independent permissions', () => {
  const Component = loadSecretAccess({});
  const blocked = [
    { secret, canManage: false, canReveal: false },
    { secret: { ...secret, credential_type: 'oauth' }, canManage: true, canReveal: true },
    { secret: { ...secret, credential_type: 'mtls' }, canManage: true, canReveal: true },
  ];
  for (const props of blocked) assert.doesNotMatch(renderToStaticMarkup(createElement(Component, props)), /<button|<dialog|<textarea/);
});

// Explicit reveal is the only network read, and close/blur remove its ephemeral result.
test('secret dialog reveals on demand and clears plaintext on blur and dismissal', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  dom.window.HTMLDialogElement.prototype.showModal = function () { this.open = true; }; // jsdom has no native top-layer implementation.
  let reads = 0;
  const Component = loadSecretAccess({
    /** Count only explicit reads; fixture material never comes from a real account. */
    async revealBucketSecret() { reads++; return { value: 'synthetic-secret' }; },
  });
  const root = createRoot(document.getElementById('root'));
  /** Locate controls by their visible label to exercise the real user interaction. */
  const button = (label) => [...document.querySelectorAll('button')].find(item => item.textContent.startsWith(label));
  try {
    await act(async () => root.render(createElement(Component, { secret, canManage: false, canReveal: true })));
    await act(async () => button('Exportable').click());
    assert.equal(reads, 0);
    assert.equal(document.querySelector('input[type="checkbox"]'), null, 'reveal does not grant management');
    await act(async () => button('Reveal secret').click());
    assert.equal(reads, 1);
    assert.equal(document.querySelector('textarea').value, 'synthetic-secret');
    assert.ok(document.querySelector('textarea').closest('[data-fused-visible="false"]'));
    await act(async () => window.dispatchEvent(new window.Event('blur')));
    assert.equal(document.querySelector('textarea'), null);
    await act(async () => button('Close').click());
    assert.equal(document.querySelector('dialog'), null);
    await act(async () => button('Exportable').click());
    assert.equal(document.querySelector('textarea'), null);
    assert.equal(reads, 1, 'reopening must not refetch or reuse plaintext');
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});

// Closing before a response arrives must not retain that response in hidden component state.
test('secret dialog cancels pending disclosure and never reveals a private secret', async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  dom.window.HTMLDialogElement.prototype.showModal = function () { this.open = true; }; // Native modal behavior is browser-owned.
  let signal, resolveRead;
  const Component = loadSecretAccess({
    /** Retain the synthetic request until after the dialog is closed. */
    revealBucketSecret(_bucket, _secret, requestSignal) { signal = requestSignal; return new Promise(resolve => { resolveRead = resolve; }); },
  });
  const root = createRoot(document.getElementById('root'));
  /** Use accessible visible controls rather than component implementation details. */
  const button = (label) => [...document.querySelectorAll('button')].find(item => item.textContent.startsWith(label));
  try {
    await act(async () => root.render(createElement(Component, { secret, canManage: true, canReveal: true })));
    await act(async () => button('Exportable').click());
    await act(async () => button('Reveal secret').click());
    await act(async () => button('Close').click());
    assert.equal(signal.aborted, true);
    await act(async () => resolveRead({ value: 'late-secret' }));
    assert.equal(document.querySelector('textarea'), null);
    await act(async () => root.render(createElement(Component, { key: 'private', secret: { ...secret, exportable: false }, canManage: true, canReveal: true })));
    await act(async () => button('Private').click());
    assert.equal(button('Reveal secret'), undefined);
    await act(async () => document.querySelector('input[type="checkbox"]').click());
    assert.equal(button('Reveal secret'), undefined, 'unsaved consent cannot enable retrieval');
  } finally {
    await act(async () => root.unmount()); dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});

// Imported API-key spelling must not suppress consent, and changing to a different family must drop it.
test('credential composer serializes explicit opt-in only for API keys and generic secrets', () => {
  const source = readFileSync(new URL('../components/buckets/BucketEntryComposerModel.ts', import.meta.url), 'utf8');
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText;
  const exports = {};
  new Function('exports', compiled)(exports);
  const form = { serviceId: 'service', value: 'synthetic', expiresAt: '', keyName: 'key' };
  const keyOption = { auth_type: 'api_key', credential_type: 'apiKey', key_name: 'key' };
  assert.equal(exports.serializeSecretPayloads(form, keyOption)[0].exportable, false);
  assert.equal(exports.serializeSecretPayloads({ ...form, exportable: true }, keyOption)[0].exportable, true);
  assert.equal(exports.serializeSecretPayloads({ ...form, exportable: true }, { ...keyOption, auth_type: 'bearer', credential_type: 'bearer' })[0].exportable, false);
  assert.equal(exports.serializeBucketSecretPayload(form).exportable, false);
  assert.equal(exports.serializeBucketSecretPayload({ ...form, exportable: true }).exportable, true);
});
