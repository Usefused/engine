import { test } from 'node:test';
import assert from 'node:assert/strict';
import { JSDOM } from 'jsdom';
import { FusedAgentPage } from './fused-agent-page.ts';
import { readFileSync } from 'node:fs';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import ts from 'typescript';
import { PersonalCredentialPanel } from '../components/access/PersonalCredentialPanel.ts';

/** Renders the actual route credential element so a missing privacy annotation fails this regression check. */
function routeCredentialMarkup(route, variable) {
  const source = readFileSync(new URL(`../routes/${route}`, import.meta.url), 'utf8');
  const element = source.match(new RegExp(`<code\\b[^>]*>\\s*\\{${variable}\\}\\s*</code>`));
  assert.ok(element, 'The production credential display must be included in the privacy check');
  const compiled = ts.transpileModule(`const markup = (${element[0]});`, { compilerOptions: { jsx: ts.JsxEmit.React } }).outputText;
  return renderToStaticMarkup(new Function('React', variable, `${compiled}; return markup;`)(React, 'PRIVATE-ISSUED-CREDENTIAL'));
}

/** Gives the reader a rendered DOM without a browser's layout engine. */
function fixture(html) {
  const dom = new JSDOM(`<main>${html}</main>`, { url: 'https://fused.example/integrations/apps' });
  for (const name of ['document', 'NodeFilter', 'HTMLInputElement', 'HTMLSelectElement', 'HTMLTextAreaElement', 'Event', 'getComputedStyle']) globalThis[name] = dom.window[name];
  dom.window.Element.prototype.getClientRects = function () { return [{ width: 100, height: 20 }]; };
  return { root: dom.window.document.querySelector('main'), location: dom.window.location, page: new FusedAgentPage() };
}

// Opt-out applies to text, controls and descendants even if a child attempts to opt back in.
test('private containers, passwords and revealed credentials never enter context', () => {
  const { page, root, location } = fixture(`<label>Name <input value="Checkout"></label>
    <section data-fused-visible="false"><span>PRIVATE-TEXT</span><input data-fused-visible="true" value="PRIVATE-INPUT"></section>
    <label>Password<input type="password" value="PASSWORD"></label>
    <label>API key<input name="api_key" type="text" value="REVEALED-KEY"></label>
    <label>Options<select data-fused-visible="false"><option value="PRIVATE-OPTION">PRIVATE-LABEL</option></select></label>
    <label>Details<span data-fused-visible="false">PRIVATE-NESTED</span><textarea>ordinary source</textarea></label>
    <a href="/integrations/apps">Apps <span data-fused-visible="false">PRIVATE-LINK</span></a>`);
  const snapshot = page.snapshot(root, location);
  assert.doesNotMatch(JSON.stringify(snapshot), /PRIVATE-|PASSWORD|REVEALED/);
  assert.equal(snapshot.fields[0].value, 'Checkout');
  assert.equal(snapshot.fields.at(-1).value, 'ordinary source');
  for (const field of snapshot.fields.filter((item) => item.private)) {
    assert.equal(field.value, undefined);
    assert.equal(field.editable, false);
    assert.throws(() => page.update(root, location, snapshot.revision, field.id, 'override'), /private/);
  }
});

// Hidden replicas and off-origin links cannot extend the agent's page capabilities.
test('context excludes hidden UI, agent messages, and external navigation', () => {
  const { page, root, location } = fixture(`<div hidden>HIDDEN</div><div style="visibility:hidden">INVISIBLE</div><div data-fused-agent><textarea>AGENT</textarea></div>
    <a href="https://evil.example/integrations/apps">Outside</a><a href="/integrations/services">Services</a>`);
  const snapshot = page.snapshot(root, location);
  assert.doesNotMatch(snapshot.text, /HIDDEN|INVISIBLE|AGENT/);
  assert.deepEqual(snapshot.links, [{ label: 'Services', path: '/integrations/services' }]);
});

// Edits require current evidence and retain actual native form events and select constraints.
test('writes use current field handles, preserve events and reject stale or protected edits', () => {
  const { page, root, location } = fixture(`<input aria-label="Name" value="Old"><select><option value="one">One</option><option value="two">Two</option></select>`);
  const first = page.snapshot(root, location);
  let changes = 0;
  root.addEventListener('input', () => { changes++; });
  const changed = page.update(root, location, first.revision, first.fields[0].id, 'New');
  assert.equal(changed.fields[0].value, 'New');
  assert.equal(changes, 1);
  assert.throws(() => page.update(root, location, first.revision, first.fields[0].id, 'Stale'), /page changed/);
  assert.throws(() => page.update(root, location, changed.revision, changed.fields[1].id, 'missing'), /available options/);
  root.querySelector('input').setAttribute('data-fused-visible', 'false');
  assert.throws(() => page.update(root, location, changed.revision, changed.fields[0].id, 'Leaked'), /page changed/);
});

// Secret query values cannot leak through either route metadata or a navigation link.
test('URL metadata omits authentication query values and protects immediate-save controls', () => {
  const { page, root, location } = fixture(`<a href="/integrations/apps?edit=app-one&access_token=PRIVATE-URL">App</a><input data-fused-editable="false" value="Immediate setting"><input name="token" value="PRIVATE-TOKEN">`);
  const snapshot = page.snapshot(root, location);
  assert.doesNotMatch(JSON.stringify(snapshot), /PRIVATE-/);
  assert.equal(snapshot.links[0].path, '/integrations/apps?edit=app-one');
  assert.equal(snapshot.fields[0].value, 'Immediate setting');
  assert.equal(snapshot.fields[0].editable, false);
});

// Busy form fieldsets lock descendants without needing disabled on every individual field.
test('inherited fieldset locks prevent edits during an existing form action', () => {
  const { page, root, location } = fixture('<fieldset disabled><input value="Locked draft"></fieldset>');
  const snapshot = page.snapshot(root, location);
  assert.equal(snapshot.fields[0].editable, false);
  assert.throws(() => page.update(root, location, snapshot.revision, snapshot.fields[0].id, 'Changed'), /unavailable/);
});

// One-time credentials must stay out of context even after the user has deliberately revealed them.
test('production account, OAuth and personal key displays omit issued values from agent context', () => {
  const markup = [
    routeCredentialMarkup('integrations.settings.tsx', 'newKey'),
    routeCredentialMarkup('integrations.access.oauth-clients.tsx', 'secret'),
    renderToStaticMarkup(React.createElement(PersonalCredentialPanel, { credentials: [], issuedSecret: 'PRIVATE-ISSUED-CREDENTIAL' })),
  ];
  for (const html of markup) {
    assert.match(html, /PRIVATE-ISSUED-CREDENTIAL/, 'Owners must still be able to see their new credential');
    const { page, root, location } = fixture(html);
    assert.doesNotMatch(JSON.stringify(page.snapshot(root, location)), /PRIVATE-ISSUED-CREDENTIAL/);
  }
});
