import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
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
    // The Node test runner cannot resolve the browser application's tilde aliases.
    if (aliases[name]) return loadComponent(aliases[name]);
    return require(name);
  }
  new Function("require", "exports", compiled)(resolve, exports);
  return exports;
}

// Exercise schema admission with real controls so invalid input and bucket scope cannot bypass execution checks.
test("runtime bucket controls preserve explicit scope and send a single scoped execution", async () => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost" });
  const previous = { window: globalThis.window, document: globalThis.document, fetch: globalThis.fetch, act: globalThis.IS_REACT_ACT_ENVIRONMENT };
  globalThis.window = dom.window; globalThis.document = dom.window.document; globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  const { createRoot } = await import("react-dom/client");
  const root = createRoot(document.getElementById("root"));
  const { UnifiedAppBucketScope } = loadComponent("../components/apps/UnifiedAppBucketScope.tsx");
  const { UnifiedAppInvocation } = loadComponent("../components/apps/UnifiedAppInvocation.tsx");
  const bucketID = "11111111-1111-4111-8111-111111111111";
  const versionID = "22222222-2222-4222-8222-222222222222";
  let scope = [];
  let defaultBucket = "";
  let finish;
  const requests = [];
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
  /** Uses native input events so the tag editor sees the same query changes as browser typing. */
  async function typeBucketQuery(value) {
    const input = document.querySelector('input[role="combobox"]');
    await act(async () => {
      Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value").set.call(input, value);
      input.dispatchEvent(new window.Event("input", { bubbles: true }));
    });
    return input;
  }
  /** Keeps the response pending to verify duplicate clicks and secret cleanup around completion. */
  globalThis.fetch = async (url, options) => {
    requests.push({ url, options });
    return new Promise((resolve) => { finish = resolve; });
  };
  try {
    await act(async () => { root.render(createElement(ScopeEditor)); });
    assert.deepEqual(scope, []);
    assert.equal(document.querySelector('input[role="combobox"]'), null);
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
    assert.equal(document.querySelector('input[role="combobox"]'), null);

    await act(async () => { root.render(createElement(UnifiedAppInvocation, { appID: "app-family", versionID, buckets: [{ id: bucketID, name: "tenant-a" }], example: '{"customer":"fabricated example"}', schema: { type: "object", required: ["customer"], properties: { customer: { type: "string", minLength: 1 } }, additionalProperties: false } })); });
    assert.equal(requests.length, 0);
    const select = document.querySelector("select");
    await act(async () => { select.value = bucketID; select.dispatchEvent(new window.Event("change", { bubbles: true })); });
    const token = document.querySelector('input[type="password"]');
    await act(async () => {
      // Use the native setter so React observes an actual input change rather than a tracked programmatic assignment.
      Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value").set.call(token, "synthetic-execution-token");
      token.dispatchEvent(new window.Event("input", { bubbles: true }));
    });
    const run = [...document.querySelectorAll("button")].find((button) => button.textContent === "Run app");
    assert.equal(run.disabled, true, "required values start unset, even when an example exists");
    const value = document.querySelector("textarea");
    await act(async () => {
      Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, "value").set.call(value, "a");
      value.dispatchEvent(new window.Event("input", { bubbles: true }));
    });
    assert.equal(run.disabled, false, "valid inline values are executable without a Save step");
    assert.equal(run.disabled, false);
    await act(async () => { run.click(); });
    assert.equal(run.disabled, true);
    await act(async () => { run.click(); });
    assert.equal(requests.length, 1);
    assert.equal(requests[0].url, "/v1/apps/app-family/executions");
    assert.deepEqual(JSON.parse(requests[0].options.body), { operation: "execute", input: { customer: "a" }, bucket_id: bucketID, expected_app_id: versionID });
    assert.equal(requests[0].options.headers.Authorization, "Bearer synthetic-execution-token");
    assert.equal(requests[0].options.credentials, "omit");
    assert.doesNotMatch(requests[0].options.body, /synthetic-execution-token/);
    await act(async () => { finish({ json: async () => ({ error: { code: "bucket_selection_denied" } }) }); });
    assert.equal(token.value, "");
    assert.match(document.querySelector("pre").textContent, /bucket_selection_denied/);
    assert.equal(requests.length, 1, "denial must not cause automatic retries");
  } finally {
    await act(async () => { root.unmount(); });
    dom.window.close();
    globalThis.window = previous.window; globalThis.document = previous.document;
    globalThis.fetch = previous.fetch; globalThis.IS_REACT_ACT_ENVIRONMENT = previous.act;
  }
});
