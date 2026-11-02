import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";
import test from "node:test";
import { createElement, createRef } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";
import { Select } from "../components/forms/Select.ts";

// Exercise real native options, including grouping and disabled choices used by page-specific menus.
function options() {
  return createElement("optgroup", { label: "Languages" },
    createElement("option", { value: "ts" }, "TypeScript"),
    createElement("option", { value: "py", disabled: true }, "Python"));
}

// The shared wrapper must preserve submission and accessibility attributes for existing forms.
test("select preserves native form semantics and configurable presentation", () => {
  const html = renderToStaticMarkup(createElement(Select, {
    id: "language", name: "language", defaultValue: "py", required: true,
    "aria-label": "Language", "aria-describedby": "language-help",
    density: "compact", tone: "subtle", className: "w-full", disabled: true,
  }, options()));
  assert.match(html, /<select/);
  assert.match(html, /name="language"/);
  assert.match(html, /required=""/);
  assert.match(html, /disabled=""/);
  assert.match(html, /aria-label="Language"/);
  assert.match(html, /aria-describedby="language-help"/);
  assert.match(html, /class="fused-select w-full"/);
  assert.match(html, /data-density="compact"/);
  assert.match(html, /data-tone="subtle"/);
  assert.match(html, /<optgroup label="Languages">/);
  assert.match(html, /<option value="py" disabled="" selected="">Python/);
  assert.doesNotMatch(html, /\s(?:density|tone)="/);
});

// Multi-selects remain native listboxes and continue selecting all supplied values.
test("select supports controlled multiple values and native size", () => {
  const html = renderToStaticMarkup(createElement(Select, {
    multiple: true, size: 4, value: ["ts", "py"], onChange() { /* Controlled fixture retains its supplied value. */ },
  }, options()));
  assert.match(html, /multiple=""/);
  assert.match(html, /size="4"/);
  assert.equal((html.match(/selected=""/g) ?? []).length, 2);
});

// Page handlers and refs must still receive the actual native element rather than an extra wrapper.
test("select forwards events and the native ref", () => {
  const ref = createRef();
  const onChange = () => { /* Identity assertion proves the page owns event handling. */ };
  const element = Select.render({ onChange, children: options(), "data-track": "select_language" }, ref);
  assert.equal(element.type, "select");
  assert.equal(element.ref, ref);
  assert.equal(element.props.onChange, onChange);
  assert.equal(element.props["data-track"], "select_language");
});

// Inventory both JSX and createElement so access controls cannot bypass the shared component.
function nativeSelects(directory) {
  const matches = [];
  for (const entry of readdirSync(directory, { withFileTypes: true })) {
    const file = new URL(entry.name, directory);
    // Recurse only through source directories; the component itself is the sole native select owner.
    if (entry.isDirectory()) { matches.push(...nativeSelects(new URL(`${entry.name}/`, directory))); continue; }
    if (!/\.tsx?$/.test(entry.name) || file.pathname.endsWith("/forms/Select.ts")) continue;
    const source = ts.createSourceFile(file.pathname, readFileSync(file, "utf8"), ts.ScriptTarget.Latest, true);
    // AST checks catch real controls without matching examples or unrelated string literals.
    function visit(node) {
      if ((ts.isJsxOpeningElement(node) || ts.isJsxSelfClosingElement(node)) && node.tagName.getText(source) === "select") matches.push(file.pathname);
      if (ts.isCallExpression(node) && /(?:^|\.)createElement$/.test(node.expression.getText(source)) && node.arguments[0]?.getText(source).match(/^["']select["']$/)) matches.push(file.pathname);
      ts.forEachChild(node, visit);
    }
    visit(source);
  }
  return matches;
}

// Keep future Fused pages on the same control instead of accumulating local caret implementations.
test("all application dropdowns use the shared select", () => {
  assert.deepEqual(nativeSelects(new URL("../", import.meta.url)), []);
});

// Exercise the actual React adapter, including controlled props, native events and StrictMode cleanup.
test("searchable select keeps React values, option updates, disabled state and native refs synchronized", async () => {
  const { JSDOM } = await import("jsdom");
  const dom = new JSDOM('<div id="root"></div>', { url: 'http://localhost' });
  const previous = Object.getOwnPropertyDescriptors(globalThis);
  // React's event system must observe the same DOM as the shared enhancement.
  for (const key of ['window', 'document', 'HTMLElement', 'Node', 'navigator']) Object.defineProperty(globalThis, key, { configurable: true, value: dom.window[key] });
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  dom.window.HTMLElement.prototype.scrollIntoView = function () {};
  const { createRoot } = await import("react-dom/client");
  const { act } = await import("react-dom/test-utils");
  const { useState, StrictMode } = await import("react");
  const ref = createRef();
  const changes = [];
  let update;
  // A real controlled parent, rather than a mocked change callback, owns the selected identity.
  function App() {
    const [state, setState] = useState({ value: 'one', disabled: false, label: 'Demo' });
    update = setState;
    return createElement('form', null,
      createElement('label', { htmlFor: 'shared-bucket' }, 'Credential bucket'),
      createElement(Select, { id: 'shared-bucket', name: 'bucket', searchable: true, required: true, ref, value: state.value, disabled: state.disabled, onChange(event) { changes.push(event.target); setState({ ...state, value: event.target.value }); } },
        createElement('option', { value: '' }, 'Choose a bucket'),
        createElement('option', { value: 'one' }, 'Default'),
        createElement('optgroup', { label: 'Blocked', disabled: true }, createElement('option', { value: 'blocked' }, 'Disabled bucket')),
        createElement('option', { value: 'two' }, state.label)));
  }
  const root = createRoot(dom.window.document.getElementById('root'));
  try {
    await act(async () => root.render(createElement(StrictMode, null, createElement(App))));
    const input = dom.window.document.querySelector('[role=combobox]');
    assert.equal(dom.window.document.querySelectorAll('[role=combobox]').length, 1);
    assert.equal(input.value, 'Default');
    assert.equal(ref.current.tagName, 'SELECT');
    assert.equal(ref.current.hidden, true);
    await act(async () => ref.current.focus());
    assert.equal(dom.window.document.activeElement, input);
    await act(async () => { input.value = 'Demo'; input.dispatchEvent(new dom.window.Event('input')); });
    await act(async () => dom.window.document.querySelector('[role=option]').click());
    assert.equal(changes[0], ref.current);
    assert.equal(ref.current.value, 'two');
    assert.deepEqual([...new dom.window.FormData(dom.window.document.querySelector('form')).entries()], [['bucket', 'two']]);
    await act(async () => update({ value: 'two', disabled: true, label: 'Renamed' }));
    assert.equal(input.disabled, true);
    assert.equal(input.value, 'Renamed');
    await act(async () => update({ value: 'one', disabled: false, label: 'New label' }));
    assert.equal(input.value, 'Default');
    await act(async () => input.click());
    const blocked = dom.window.document.querySelector('[aria-disabled=true]');
    assert.ok(blocked);
    await act(async () => blocked.click());
    assert.equal(ref.current.value, 'one');
    await act(async () => input.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'ArrowDown', cancelable: true })));
    assert.equal(dom.window.document.getElementById(input.getAttribute('aria-activedescendant')).textContent, 'New label');
    await act(async () => input.dispatchEvent(new dom.window.KeyboardEvent('keydown', { key: 'Enter', cancelable: true })));
    assert.equal(ref.current.value, 'two');
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
    // Restore process globals so later tests do not inherit a closed document or act setting.
    for (const key of ['window', 'document', 'HTMLElement', 'Node', 'navigator', 'IS_REACT_ACT_ENVIRONMENT']) {
      if (previous[key]) Object.defineProperty(globalThis, key, previous[key]);
      else delete globalThis[key];
    }
  }
});
