import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { renderToStaticMarkup } from "react-dom/server";
import { JSDOM } from "jsdom";
import ts from "typescript";
import * as permissions from "./current-actor-permissions.ts";

const require = createRequire(import.meta.url);
const source = readFileSync(new URL("../components/webhooks/WebhookMessageDiagnostics.tsx", import.meta.url), "utf8");
const event = { id: "receipt", service_id: "service", registration_id: "registration", verification_status: "rejected" };
const service = { permission: "service.read", resource_type: "SERVICE", resource_id: "service" };
const audit = { permission: "audit.read", resource_type: "WORKSPACE", resource_id: "workspace" };
const diagnostic = { permission: "service.webhook.diagnostics.read", resource_type: "SERVICE", resource_id: "service" };

// access creates synthetic, exact-resource grants without consulting a real Engine.
function access(grants) { return { workspace_id: "workspace", grants, authorization_revision: 1 }; }

// loadInspector exercises production JSX and effects with controlled access and a private transport spy.
function loadInspector(getAccess, reveal) {
  const code = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  // Local dependencies remain real except the actor and network boundaries under test.
  function resolve(name) {
    if (name === "~/components/access/CurrentActorAccess") return { useCurrentActorAccess: getAccess };
    if (name === "~/lib/current-actor-access") return permissions;
    if (name === "~/lib/api") return { api: { workspace: { webhookDiagnostics: reveal } } };
    return require(name);
  }
  new Function("require", "exports", code)(resolve, exports);
  return exports.WebhookMessageDiagnostics;
}

// Independent grants and resolved access are all necessary; ordinary receipt access never admits a message.
test("private webhook inspector permission matrix and initial render never fetch bodies", () => {
  let calls = 0;
  let state;
  // Counting network access makes hidden-prefetch regressions observable.
  const Inspector = loadInspector(() => state, () => { calls++; });
  for (const grants of [[], [service], [audit], [diagnostic], [service, audit], [service, diagnostic], [audit, diagnostic], [service, audit, { ...diagnostic, resource_id: "other" }]]) {
    state = { access: access(grants), loading: false, failed: false };
    assert.equal(renderToStaticMarkup(React.createElement(Inspector, { event })), "");
  }
  for (const unavailable of [{ access: null }, { loading: true }, { failed: true }]) {
    state = { access: access([service, audit, diagnostic]), ...unavailable };
    assert.equal(renderToStaticMarkup(React.createElement(Inspector, { event })), "");
  }
  state = { access: access([service, audit, diagnostic]) };
  assert.equal(renderToStaticMarkup(React.createElement(Inspector, { event: { ...event, registration_id: "" } })), "");
  const html = renderToStaticMarkup(React.createElement(Inspector, { event }));
  assert.match(html, /Private webhook message/);
  assert.match(html, /aria-expanded="false"/);
  assert.match(html, /data-fused-visible="false"/);
  assert.equal(calls, 0);
});

// Real React effects prove lazy retrieval, cancellation, stale-response rejection and permission-change unmounting.
test("reveal is exact scoped and collapse drops private bytes, including late responses", async () => {
  const dom = new JSDOM("<!doctype html><div id='root'></div>");
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  let state = { access: access([service, audit, diagnostic]) };
  const calls = [];
  // Retain resolvers to deliberately deliver one response after the user has collapsed its section.
  const Inspector = loadInspector(() => state, (...args) => new Promise((resolve) => calls.push({ args, resolve })));
  const root = createRoot(document.getElementById("root"));
  // Rendering and clicking use actual DOM events, not an implementation-shaped state mock.
  const render = () => root.render(React.createElement(Inspector, { event }));
  const click = () => document.querySelector("button").click();
  const detail = { body: '<script>PRIVATE_SENTINEL</script>', encoding: "utf-8", method: "POST", content_type: "application/json", original_bytes: 40, captured_bytes: 40, truncated: false };
  try {
    await act(async () => render());
    assert.equal(calls.length, 0);
    await act(async () => click());
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].args.slice(0, 3), ["service", "registration", "receipt"]);
    await act(async () => click());
    assert.equal(calls[0].args[3].aborted, true);
    await act(async () => calls[0].resolve(detail));
    assert.doesNotMatch(document.body.textContent, /PRIVATE_SENTINEL/);
    await act(async () => click());
    assert.equal(calls.length, 2);
    await act(async () => calls[1].resolve(detail));
    assert.match(document.body.textContent, /PRIVATE_SENTINEL/);
    assert.match(document.body.textContent, /Unverified message/);
    assert.equal(document.querySelector("script"), null);
    await act(async () => click());
    assert.doesNotMatch(document.body.textContent, /PRIVATE_SENTINEL/);
    await act(async () => click());
    await act(async () => calls[2].resolve({ ...detail, truncated: true }));
    assert.match(document.body.textContent, /Truncated/);
    state = { access: access([service, audit]) };
    await act(async () => render());
    assert.equal(document.querySelector("button"), null);
    assert.doesNotMatch(document.body.textContent, /PRIVATE_SENTINEL/);
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
    delete globalThis.window;
    delete globalThis.document;
    delete globalThis.IS_REACT_ACT_ENVIRONMENT;
  }
});
