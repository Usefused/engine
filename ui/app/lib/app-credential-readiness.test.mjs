import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { webcrypto } from "node:crypto";
import test from "node:test";
import ts from "typescript";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import * as contract from "./app-builder-contract.ts";
import * as readiness from "./app-credential-readiness.ts";

const require = createRequire(import.meta.url);
const missing = { buckets: [{ id: "billing-id", name: "billing" }], missing_credentials: [{ service_id: "stripe-id", service: "@payments/stripe", bucket_id: "billing-id", bucket_name: "billing", auth_type: "bearer", auth_name: "bearerAuth", required_fields: [{ name: "token", secret_key: "bearerAuth" }] }] };

/** Transpiles production modules with a recorded transport while keeping their real serialization and UI. */
function load(relative, dependencies) {
  const source = readFileSync(new URL(relative, import.meta.url), "utf8");
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const exports = {};
  new Function("require", "exports", "crypto", code)((name) => dependencies[name] ?? require(name), exports, webcrypto);
  return exports;
}

/** Creates distinguishable receipts so a readiness recheck cannot accidentally apply the first plan. */
function plan(id, credentials = missing) {
  return { plan_id: id, source_hash: `sha256:${id}`, owner_type: "subject", config_key: "app:checkout", summary: {}, credential_readiness: credentials };
}

/** Records only plan and apply boundaries; tests must never retrieve or write credential values. */
function client(plans) {
  const calls = [];
  const api = { appConfig: {
    // Each recheck returns a fresh immutable receipt from the fake Engine.
    plan: async (kind, input) => { calls.push({ action: "plan", kind, input }); return plans.shift(); },
    // Apply records exactly the approved receipt, matching the production transport contract.
    apply: async (kind, input) => { calls.push({ action: "apply", kind, input }); return { app_id: "created" }; },
  } };
  return { calls, ...load("./app-builder.ts", { "./api": { api }, "./app-builder-contract": contract }) };
}

// Both hosted MCP and packaged/direct SDK modes must stop before apply until the user resolves the warning.
test("SDK and MCP publication waits for credential review", async () => {
  for (const kind of ["sdk", "mcp"]) {
    const receipt = plan("reviewed"), app = client([receipt]);
    let release, entered;
    const waiting = new Promise((resolve) => { entered = resolve; });
    const result = app.planAndApplyApp(kind, "team", { name: "checkout", version: "1.0.0" }, (value) => {
      assert.equal(value, receipt); entered();
      return new Promise((resolve) => { release = resolve; });
    });
    await waiting;
    assert.equal(app.calls.filter((call) => call.action === "apply").length, 0);
    release(receipt);
    assert.deepEqual(await result, { app_id: "created" });
    assert.deepEqual(app.calls.at(-1), { action: "apply", kind, input: { plan_id: "reviewed", source_hash: "sha256:reviewed" } });
  }
});

// Cancelling or visiting bucket setup must leave the app unpublished and the form available for edits.
test("cancelling credential review never applies a plan", async () => {
  const app = client([plan("cancelled")]);
  assert.equal(await app.planAndApplyApp("sdk", "", { name: "checkout", version: "1.0.0" }, async () => null), null);
  assert.deepEqual(app.calls.map((call) => call.action), ["plan"]);
});

// Rechecking after bucket setup must publish the new receipt, preserving owner and config intent.
test("rechecking applies only the fresh credential-reviewed receipt", async () => {
  const app = client([plan("before"), plan("after", null)]);
  await app.planAndApplyApp("mcp", "team", { name: "checkout", version: "1.0.0" }, async (_, recheck) => recheck());
  assert.deepEqual(app.calls.map((call) => call.action), ["plan", "plan", "apply"]);
  assert.deepEqual(app.calls[0].input, app.calls[1].input);
  assert.deepEqual(app.calls[2].input, { plan_id: "after", source_hash: "sha256:after" });
});

// Rechecking credentials must not reset the user's independent token-issuance choice.
test("token opt-out survives credential rechecking without entering immutable config", async () => {
  for (const kind of ["sdk", "mcp"]) {
    const app = client([plan("before"), plan("after", null)]);
    await app.planAndApplyApp(kind, "", { name: "checkout", version: "1.0.0" }, async (_, recheck) => recheck(), false);
    assert.deepEqual(app.calls.at(-1).input, { plan_id: "after", source_hash: "sha256:after", skip_token: true });
    assert.equal("skip_token" in app.calls[0].input.config, false);
  }
});

// Unified App deployment uses the same reviewed apply request and initial-token preference.
test("Unified App apply supports token generation and opt-out", async () => {
  const app = client([]), receipt = plan("compiled", null);
  await app.applyApp("unified-app", receipt, false);
  assert.deepEqual(app.calls.at(-1), { action: "apply", kind: "unified-app", input: { plan_id: "compiled", source_hash: "sha256:compiled", skip_token: true } });
  await app.applyApp("unified-app", receipt, true);
  assert.deepEqual(app.calls.at(-1).input, { plan_id: "compiled", source_hash: "sha256:compiled" });
});

// The shared checkbox exposes an accessible opt-in state without controlling immutable app content.
test("execution token option reflects the selected issuance preference", () => {
  const { ExecutionTokenOption } = load("../components/apps/ExecutionTokenOption.tsx", {});
  const enabled = renderToStaticMarkup(createElement(ExecutionTokenOption, { checked: true, onChange: () => {} }));
  const disabled = renderToStaticMarkup(createElement(ExecutionTokenOption, { checked: false, disabled: true, onChange: () => {} }));
  assert.match(enabled, /Generate execution token/);
  assert.match(enabled, /checked=""/);
  assert.doesNotMatch(disabled, /checked=/);
  assert.match(disabled, /disabled=""/);
});

// A readiness lookup failure is not consent to publish with unknown credentials.
test("failed readiness review does not apply", async () => {
  const app = client([plan("before")]);
  await assert.rejects(app.planAndApplyApp("sdk", "", { name: "checkout", version: "1.0.0" }, async () => { throw new Error("check unavailable"); }), /check unavailable/);
  assert.equal(app.calls.some((call) => call.action === "apply"), false);
});

// A plan without missing requirements covers ready, anonymous and broker-managed services without speculative warnings.
test("readiness and bucket links use resolved credential metadata", () => {
  assert.deepEqual(readiness.missingAppCredentials(null), []);
  assert.deepEqual(readiness.missingAppCredentials({ missing_credentials: [], buckets: [] }), []);
  const item = missing.missing_credentials[0];
  assert.equal(new URL(readiness.appCredentialBucketURL(item), "https://engine.example").searchParams.get("bucket"), "billing-id");
  assert.notEqual(readiness.appCredentialRequirementKey(item), readiness.appCredentialRequirementKey({ ...item, bucket_id: "default-id" }));
});

// The rendered warning identifies human-readable services/buckets and offers administration only with access.
test("warning respects bucket-management access and preserves the draft through a new tab", () => {
  const { AppCredentialWarning } = load("../components/apps/AppCredentialWarning.tsx", { "~/lib/app-credential-readiness": readiness });
  const allowed = renderToStaticMarkup(createElement(AppCredentialWarning, { readiness: missing, canManageBucket: () => true }));
  assert.match(allowed, /@payments\/stripe/);
  assert.match(allowed, /bearerAuth/);
  assert.match(allowed, /billing/);
  assert.match(allowed, /target="_blank"/);
  assert.match(allowed, /bucket=billing-id/);
  const denied = renderToStaticMarkup(createElement(AppCredentialWarning, { readiness: missing, canManageBucket: () => false }));
  assert.doesNotMatch(denied, /href=/);
  assert.match(denied, /Ask a bucket administrator/);
  assert.equal(renderToStaticMarkup(createElement(AppCredentialWarning, { readiness: null, canManageBucket: () => true })), "");
});
