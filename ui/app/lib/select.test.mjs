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
