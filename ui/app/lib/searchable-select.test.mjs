import test from 'node:test';
import assert from 'node:assert/strict';
import { JSDOM } from 'jsdom';
import { enhanceSearchableSelect } from '../../branding/searchable_select.js';

// Reset and teardown must restore native semantics in both React and standalone forms.
test('shared search validates unmatched text, follows reset and restores labels on teardown', async () => {
 const dom = new JSDOM('<form><label for="bucket">Bucket</label><select id="bucket" name="bucket" required><option value="a">Alpha</option><option value="b">Beta</option></select></form>');
 dom.window.HTMLElement.prototype.scrollIntoView = function () {};
 const select = dom.window.document.querySelector('select');
 const control = enhanceSearchableSelect(select);
 const input = dom.window.document.querySelector('input');
 input.value = 'unknown'; input.dispatchEvent(new dom.window.Event('input'));
 assert.equal(select.value, '');
 assert.equal(select.checkValidity(), false);
 assert.equal(input.checkValidity(), false);
 dom.window.document.querySelector('form').reset();
 await new Promise(resolve => dom.window.queueMicrotask(resolve));
 assert.equal(input.value, 'Alpha');
 assert.equal(select.checkValidity(), true);
 control.destroy();
 assert.equal(select.hidden, false);
 assert.equal(dom.window.document.querySelector('label').htmlFor, 'bucket');
 assert.equal(dom.window.document.querySelector('[role=combobox]'), null);
 dom.window.close();
});

// An existing listbox must keep its multi-value form contract even when a caller opts into search.
test('shared search leaves native multi-selects and listboxes intact', () => {
 const dom = new JSDOM('<select multiple><option selected value="a">Alpha</option><option selected value="b">Beta</option></select>');
 const select = dom.window.document.querySelector('select');
 const control = enhanceSearchableSelect(select);
 assert.equal(select.hidden, false);
 assert.equal(dom.window.document.querySelector('[role=combobox]'), null);
 assert.equal(select.selectedOptions.length, 2);
 control.destroy();
 dom.window.close();
});
