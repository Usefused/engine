import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { JSDOM } from 'jsdom';
import { enhanceSearchableSelect } from '../../branding/searchable_select.js';

const script = readFileSync(new URL('../../branding/oauth_consent.js', import.meta.url), 'utf8');

// Model the actual progressive form contract, including the authoritative native UUID field.
function fixture() {
  const dom = new JSDOM(`<form><label for="runtime-default-bucket">Default credential bucket</label><select id="runtime-default-bucket" class="fused-select" name="bucket" required data-searchable><option value="">Choose a bucket</option><option value="one">default</option><option value="two">demo</option></select><input type="checkbox" name="runtime_bucket" value="extra" checked></form>`, { runScripts: 'outside-only' });
  dom.window.HTMLElement.prototype.scrollIntoView = function () {};
  dom.window.enhanceSearchableSelect = enhanceSearchableSelect;
  dom.window.eval(script);
  const document = dom.window.document;
  return { dom, document, input: document.querySelector('[role=combobox]'), select: document.querySelector('select'), menu: document.querySelector('.fused-select-menu') };
}

// Filtering and selection retain only the authorized option value and preserve independent approved extras.
test('bucket search selects an exact UUID and invalidates edited text', () => {
  const { dom, document, input, select, menu } = fixture();
  input.focus();
  input.value = 'DEM';
  input.dispatchEvent(new dom.window.Event('input'));
  assert.equal(document.querySelectorAll('[role=option]').length, 1);
  assert.equal(document.querySelector('[role=option]').textContent, 'demo');
  assert.equal(input.checkValidity(), false);
  document.querySelector('[role=option]').click();
  assert.equal(select.value, 'two');
  assert.equal(input.value, 'demo');
  assert.equal(menu.hidden, true);
  assert.equal(input.checkValidity(), true);
  assert.deepEqual([...new dom.window.FormData(document.querySelector('form')).entries()], [['bucket', 'two'], ['runtime_bucket', 'extra']]);
  input.value = 'nonexistent';
  input.dispatchEvent(new dom.window.Event('input'));
  assert.equal(select.value, '');
  assert.equal(input.checkValidity(), false);
  assert.equal(document.querySelector('.fused-select-empty').hidden, false);
  dom.window.close();
});

// Arrow keys and Enter choose an option without triggering consent, and Escape dismisses the menu.
test('bucket keyboard selection never implicitly submits OAuth consent', () => {
  const { dom, document, input, select, menu } = fixture();
  input.focus();
  input.value = 'demo';
  input.dispatchEvent(new dom.window.Event('input'));
  input.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'ArrowDown', cancelable: true }));
  const enter = new dom.window.KeyboardEvent('keydown', { key: 'Enter', cancelable: true });
  input.dispatchEvent(enter);
  assert.equal(enter.defaultPrevented, true);
  assert.equal(select.value, 'two');
  assert.equal(menu.hidden, true);
  input.click();
  assert.equal(document.querySelectorAll('[role=option]').length, 2);
  input.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Escape' }));
  assert.equal(input.getAttribute('aria-expanded'), 'false');
  dom.window.close();
});
