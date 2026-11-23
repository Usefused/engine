import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import ts from "typescript";
import { act, createElement, useState } from "react";
import { JSDOM } from "jsdom";

import * as fields from "./unified-app-input-fields.ts";
import * as validation from "./schedule-input-validation.ts";
const require = createRequire(import.meta.url);
const aliases = {
  "./UnifiedAppInput": "../components/apps/UnifiedAppInput.tsx",
  "~/components/forms/Select": "../components/forms/Select.ts",
  "~/components/CopyValue": "../components/CopyValue.tsx",
  "~/lib/app-execution-url": "./app-execution-url.ts",
  "~/lib/unified-app-runtime-buckets": "./unified-app-runtime-buckets.ts",
};

/** Loads production controls with their real dependencies without needing Vite's alias resolver. */
function loadComponent(path) {
  const source = readFileSync(new URL(path, import.meta.url), "utf8");
  const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  /** Resolves only known application aliases; React and third-party modules remain unchanged. */
  function resolve(name) {
    // Schema extraction and validation use their real ESM implementations.
    if (name === "~/lib/unified-app-input-fields") return fields;
    if (name === "~/lib/schedule-input-validation") return validation;
    // Transpiled component imports resolve from this test file, so restore the production branding path explicitly.
    if (name === "../../../branding/searchable_select.js") return require(fileURLToPath(new URL("../../branding/searchable_select.js", import.meta.url)));
    // The Node test runner cannot resolve the browser application's tilde aliases.
    if (aliases[name]) return loadComponent(aliases[name]);
    return require(name);
  }
  new Function("require", "exports", compiled)(resolve, exports);
  return exports;
}

// Exercise runtime bucket scope controls so invalid free-text buckets cannot bypass allowed selections.
test("runtime bucket controls preserve explicit scope and default routing", async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost" });
  const previous = { window: globalThis.window, document: globalThis.document, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { createRoot } = await import("react-dom/client");
  const root = createRoot(document.getElementById("root"));
  const { UnifiedAppBucketScope } = loadComponent("../components/apps/UnifiedAppBucketScope.tsx");
  let scope = [];
  let defaultBucket = "";
  /** Mirrors the editor's controlled configuration without granting any bucket permissions locally. */
  function ScopeEditor() {
    const [value, setValue] = useState([]);
    const [bucket, setBucket] = useState("bucket-b");
    scope = value; defaultBucket = bucket;
    return createElement(UnifiedAppBucketScope, { value, bucket, buckets: [{ resource_id: "bucket-a", display_name: "tenant-a" }, { resource_id: "bucket-b", display_name: "tenant-b" }], onBucketChange: setBucket, onChange: setValue });
  }
  /** Exercises the native dropdown event used for both routing mode and default bucket changes. */
  async function chooseBucket(value, selector = "#unified-credential-bucket") {
    const input = document.querySelector(selector);
    await act(async () => { input.value = value; input.dispatchEvent(new window.Event("change", { bubbles: true })); });
  }
  /** Targets the runtime tag editor rather than the searchable fixed-bucket select. */
  async function typeBucketQuery(value) {
    const input = document.querySelector('input[placeholder="Add a bucket…"]');
    await act(async () => {
      Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value").set.call(input, value);
      input.dispatchEvent(new window.Event("input", { bubbles: true }));
    });
    return input;
  }
  try {
    await act(async () => { root.render(createElement(ScopeEditor)); });
    assert.deepEqual(scope, []);
    // Fixed routing hides only the runtime tag editor; the bucket select remains searchable.
    assert.equal(document.querySelector('input[placeholder="Add a bucket…"]'), null);
    await chooseBucket("__fused_runtime_bucket__");
    assert.deepEqual(scope, ["tenant-b"], "runtime selection starts from the configured default");
    assert.equal(defaultBucket, "bucket-b");
    assert.equal(document.querySelector('button[aria-label="Remove tenant-b"]').disabled, true, "the last permitted bucket cannot silently disable runtime mode");
    let tags = await typeBucketQuery("unavailable-bucket");
    await act(async () => { tags.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true })); });
    assert.deepEqual(scope, ["tenant-b"], "free text must not become an authorized bucket");
    tags = await typeBucketQuery("tenant-a");
    await act(async () => { tags.dispatchEvent(new window.KeyboardEvent("keydown", { key: "Enter", bubbles: true })); });
    assert.deepEqual(scope, ["tenant-b", "tenant-a"]);
    assert.equal(tags.value, "");
    await act(async () => { document.querySelector('button[aria-label="Remove tenant-b"]').click(); });
    assert.deepEqual(scope, ["tenant-a"]);
    await typeBucketQuery("tenant-b");
    await act(async () => { document.querySelector('button[role="option"]').click(); });
    assert.deepEqual(scope, ["tenant-a", "tenant-b"], "a suggestion click adds a tag");
    await act(async () => { document.querySelector('button[aria-label="Remove tenant-b"]').click(); });
    await chooseBucket("bucket-a", 'label select');
    assert.equal(defaultBucket, "bucket-a");
    assert.deepEqual(scope, ["tenant-a"]);
    await chooseBucket("bucket-b");
    assert.deepEqual(scope, [], "a fixed dropdown choice disables runtime selection");
    assert.equal(defaultBucket, "bucket-b");
    // Returning to a fixed bucket removes the runtime tag editor again.
    assert.equal(document.querySelector('input[placeholder="Add a bucket…"]'), null);
  } finally {
    await act(async () => { root.unmount(); });
    dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document;
    globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
