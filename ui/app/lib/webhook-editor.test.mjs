import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import ts from "typescript";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import * as draftContract from "./webhook-editor-draft.ts";
import * as importContract from "./webhook-editor-import.ts";
import { APIRequestError } from "./authorization-error.ts";
import * as apiErrors from "./authorization-error.ts";
import * as browserRequest from "./browser-request.ts";
import { parseWebhookEditorJSON } from "./webhook-editor-json.ts";

const require = createRequire(import.meta.url);

// Execute production modules with only their IO/hooks substituted by deterministic fixtures.
function loadModule(path, modules) {
  const source = readFileSync(new URL(path, import.meta.url), "utf8");
  const output = ts.transpileModule(source, { fileName: path, compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const module = { exports: {} };
  const dependency = (name) => modules[name] ?? require(name);
  new Function("require", "module", "exports", output)(dependency, module, module.exports);
  return module.exports;
}

// Synthetic event metadata exercises preservation without reading any workspace credentials.
function sourceDocument() {
  return {
    openapi: "3.1.0", info: { title: "Test", version: "1" }, paths: {},
    components: { schemas: { Event: { type: "object", properties: { id: { type: "string" } } } } },
    "x-fused-event-discriminator": ["body.type", "body.action"],
    "x-fused-signature-policy": { version: "1", rules: [{ method: "synthetic" }] },
    webhooks: { "Issue:Created": { parameters: [{ name: "X-Provider", in: "header", schema: { type: "string" } }], post: { summary: "Created", description: "Full provider description", security: [{ providerAuth: [] }], requestBody: { content: { "application/json": { schema: { $ref: "#/components/schemas/Event" }, example: { id: "example" } } } }, responses: { "204": { description: "Accepted" } }, "x-provider-detail": { preserve: true } } } },
  };
}

// Editing permission requires both verified Registry ownership and the exact workspace import grant.
test("webhook editing fails closed for unknown ownership and missing grants", () => {
  const access = { workspace_id: "workspace", grants: [{ permission: "catalogue.import", resource_type: "WORKSPACE", resource_id: "workspace" }] };
  assert.equal(draftContract.canEditWebhook(true, access), true);
  assert.equal(draftContract.canEditWebhook(undefined, access), false);
  assert.equal(draftContract.canEditWebhook(false, access), false);
  assert.equal(draftContract.canEditWebhook(true, null), false);
  assert.equal(draftContract.canEditWebhook(true, { ...access, workspace_id: "other" }), false);
});

// A settings-only edit must retain every event, schema reference, security field, and advanced policy.
test("lossless draft round trip preserves references and unedited metadata", () => {
  const document = sourceDocument();
  const draft = draftContract.readWebhookDraft(JSON.stringify(document));
  assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(draft)), document);
  const edited = draftContract.updateWebhookSetting(draft, "x-fused-event-discriminator", "header.X-Event");
  assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(edited)), { ...document, "x-fused-event-discriminator": "header.X-Event" });
});

// Event labels are provider identifiers, never slugs; the displayed summary is the field intentionally edited.
test("event rename and description preserve advanced operation data", () => {
  const draft = draftContract.readWebhookDraft(JSON.stringify(sourceDocument()));
  draft.events[0] = { ...draft.events[0], name: "Issue:Updated", description: "Updated" };
  const result = JSON.parse(draftContract.serializeWebhookDraft(draft));
  assert.equal(result.webhooks["Issue:Updated"].post.summary, "Updated");
  assert.equal(result.webhooks["Issue:Updated"].post.description, "Full provider description");
  assert.equal(result.webhooks["Issue:Updated"].post["x-provider-detail"].preserve, true);
  assert.equal(result.webhooks["Issue:Created"], undefined);
});

// Explicit payload editing never infers required fields from an example or expands a reference.
test("optional schema and example edits remain independent", () => {
  const draft = draftContract.readWebhookDraft(JSON.stringify(sourceDocument()));
  draft.events[0].exampleText = '{"id":"changed"}';
  const json = JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["Issue:Created"].post.requestBody.content["application/json"];
  assert.deepEqual(json.schema, { $ref: "#/components/schemas/Event" });
  assert.deepEqual(json.example, { id: "changed" });
  draft.events[0].schemaText = "invalid JSON";
  assert.throws(() => draftContract.serializeWebhookDraft(draft));
});

// Duplicate object keys and prototype-looking provider names must not silently overwrite the catalogue.
test("duplicates fail and prototype-looking names remain ordinary events", () => {
  const draft = draftContract.readWebhookDraft(JSON.stringify(sourceDocument()));
  draft.events.push({ ...draft.events[0], id: "second" });
  assert.throws(() => draftContract.serializeWebhookDraft(draft), /Duplicate event/);
  draft.events = [{ ...draft.events[0], name: "__proto__" }];
  assert.equal(JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks.__proto__.post.summary, "Created");
});

// Shared path references require a richer editor rather than a lossy reconstruction.
test("unsafe reference editing and oversized drafts fail closed", () => {
  assert.throws(() => draftContract.readWebhookDraft(JSON.stringify({ webhooks: { event: { $ref: "#/components/pathItems/Shared" } } })), /Referenced webhook/);
  assert.throws(() => draftContract.readWebhookDraft(" ".repeat(draftContract.webhookEditorMaxBytes + 1)), /4 MiB/);
});

// Empty catalogues are intentional complete documents, allowing first-event authoring and reviewed deletion.
test("empty and event-only catalogues do not require fabricated payload schemas", () => {
  const draft = draftContract.readWebhookDraft('{"openapi":"3.1.0","webhooks":{}}');
  assert.equal(draft.events.length, 0);
  draft.events.push({ ...draftContract.newWebhookEvent(), name: "billing.created" });
  const operation = JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["billing.created"].post;
  assert.equal(operation.requestBody, undefined);
});

// Builder and file source use exactly the same destination-bound import request contract.
test("webhook plans carry explicit version and baseline revision", () => {
  const baseline = { service_id: "service", service_version_id: "version-id", revision: 7, source_content: "{}" };
  const input = importContract.webhookImportInput({ name: "Test", slug: "test" }, "v1", baseline, "source");
  assert.deepEqual(input.expected_target, { service_id: "service", service_version_id: "version-id", revision: 7 });
  assert.equal(input.destination_version, "v1");
  assert.equal(input.target_type, "webhooks");
  assert.equal(input.include_webhook_draft, true);
  const plan = { is_new_service: false, service_id: "service", target_version: "v1", action: "update_version", expected_target: input.expected_target };
  assert.doesNotThrow(() => importContract.assertWebhookPlanTarget(plan, baseline, "v1"));
  assert.throws(() => importContract.assertWebhookPlanTarget({ ...plan, is_new_service: true }, baseline, "v1"));
  assert.throws(() => importContract.assertWebhookPlanTarget({ ...plan, target_version: "v2" }, baseline, "v1"));
  assert.throws(() => importContract.assertWebhookPlanTarget({ ...plan, expected_target: undefined }, baseline, "v1"), /baseline revision/);
  assert.throws(() => importContract.assertWebhookPlanTarget({ ...plan, expected_target: { ...input.expected_target, revision: 6 } }, baseline, "v1"), /baseline revision/);
  assert.throws(() => importContract.assertWebhookPlanTarget({ ...plan, expected_target: { ...input.expected_target, service_version_id: "other" } }, baseline, "v1"), /baseline revision/);
});

// A timeout does not imply rollback, while confirmed failures can safely return to review.
test("apply errors preserve honest status recovery and durable destination checks", () => {
  assert.equal(importContract.webhookApplyNeedsStatus(new Error("network interrupted")), true);
  assert.equal(importContract.webhookApplyNeedsStatus(new APIRequestError(409, { commit_state: "not_committed" })), false);
  const baseline = { service_id: "service", service_version_id: "version-id", revision: 7 };
  const status = { status: "applied", commit_state: "committed", service_id: "service", service_version_id: "version-id", version: "v1", revision: 8 };
  assert.equal(importContract.webhookStatusCommitted(status, baseline, "v1"), true);
  assert.equal(importContract.webhookStatusCommitted({ ...status, revision: undefined }, baseline, "v1"), false);
  assert.equal(importContract.webhookStatusCommitted({ ...status, service_id: "other" }, baseline, "v1"), false);
  assert.equal(importContract.webhookStatusCommitted({ ...status, revision: 7 }, baseline, "v1"), false);
  assert.doesNotThrow(() => importContract.assertWebhookApplyResult(status, baseline, "v1"));
  assert.throws(() => importContract.assertWebhookApplyResult({ ...status, service_version_id: undefined }, baseline, "v1"));
  assert.throws(() => importContract.assertWebhookApplyResult({ ...status, revision: 7 }, baseline, "v1"));
});

// Real event form markup contains schema authoring but never token or bucket credential controls.
test("event forms and advanced policy view contain no credential inputs", () => {
  const events = loadModule("../components/webhooks/WebhookEventEditor.tsx", { "~/lib/webhook-editor-draft": draftContract });
  const settings = loadModule("../components/webhooks/WebhookSettingsEditor.tsx", { "~/lib/webhook-editor-draft": draftContract, "./WebhookEventEditor": events });
  const draft = draftContract.readWebhookDraft(JSON.stringify(sourceDocument()));
  const markup = renderToStaticMarkup(createElement(settings.WebhookSettingsEditor, { draft, onChange() {}, onInvalid() {} }));
  assert.match(markup, /Advanced signature policy \(preserved\)/);
  assert.match(markup, /credential buckets/);
  assert.doesNotMatch(markup, /type="password"|bucket_id|signing_secret|secret_value/);
  const eventMarkup = renderToStaticMarkup(createElement(events.WebhookEventEditor, { draft, onChange() {} }));
  assert.match(eventMarkup, /Issue:Created/);
  assert.match(eventMarkup, /JSON Schema/);
});

// Developer-facing controls expose supported choices without turning routine event authoring into a tutorial.
test("new events default to POST with only supported methods under advanced delivery settings", () => {
  const events = loadModule("../components/webhooks/WebhookEventEditor.tsx", { "~/lib/webhook-editor-draft": draftContract });
  const event = { ...draftContract.newWebhookEvent(), name: "invoice.deleted" };
  const draft = { document: { openapi: "3.1.0", webhooks: {} }, events: [event] };
  const markup = renderToStaticMarkup(createElement(events.WebhookEventEditor, { draft, onChange() {} }));
  assert.equal(event.method, "post");
  assert.match(markup, /<details[^>]*><summary[^>]*>Delivery method: POST · Advanced<\/summary>/);
  assert.doesNotMatch(markup, /<details[^>]*\bopen(?:=|\s|>)[^>]*><summary[^>]*>Delivery method: POST/);
  assert.deepEqual([...markup.matchAll(/<option value="([^"]+)"/g)].map((match) => match[1]), ["post", "get"]);
  assert.doesNotMatch(markup, /does not require HTTP DELETE|Use exact provider event names|Keep POST unless/);
  assert.equal(JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["invoice.deleted"].post.responses["200"].description, "Webhook accepted");
});

// Imported contracts retain their methods and a concise warning about actual receiver limitations.
test("imported delivery methods remain exact and unsupported methods show an explicit warning", () => {
  const events = loadModule("../components/webhooks/WebhookEventEditor.tsx", { "~/lib/webhook-editor-draft": draftContract });
  for (const method of draftContract.webhookMethods) {
    const document = { openapi: "3.1.0", webhooks: { "invoice.deleted": { [method]: { summary: "Deleted", responses: { "204": { description: "Accepted" } }, "x-provider-detail": { preserve: true } } } } };
    const draft = draftContract.readWebhookDraft(JSON.stringify(document));
    const markup = renderToStaticMarkup(createElement(events.WebhookEventEditor, { draft, onChange() {} }));
    assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(draft)), document);
    // Imported overrides should be visible immediately rather than hidden behind the POST-first disclosure.
    if (method !== "post") assert.match(markup, /<details[^>]*\bopen=""/);
    // Explicitly documented GET is supported, so only other verbs need the preserved-but-not-executable warning.
    if (!["post", "get"].includes(method)) {
      assert.match(markup, new RegExp(`<option value="${method}" disabled="" selected="">`));
      assert.match(markup, /role="note"/);
      assert.match(markup, /unsupported by Engine ingress \(POST\/GET only\)/);
    } else {
      assert.doesNotMatch(markup, /imported; unsupported/);
    }
    // Editing an unrelated field cannot implicitly migrate the imported transport contract.
    draft.events[0].description = "Updated description";
    assert.deepEqual(Object.keys(JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["invoice.deleted"]), [method]);
  }
});

// Walk the real hook-free event form to exercise its callbacks instead of duplicating the selector's state transitions.
function eventFormControls(element) {
  // Primitive children are display copy, not interactive controls.
  if (!element || typeof element !== "object") return [];
  // Arrays and function components are expanded exactly far enough to reach their host controls.
  if (Array.isArray(element)) return element.flatMap(eventFormControls);
  if (typeof element.type === "function") return eventFormControls(element.type(element.props));
  return [element, ...eventFormControls(element.props?.children)];
}

// A method edit and its explicit undo must retain payloads and source metadata while using the parent's normal dirty/review path.
test("actual method callbacks preserve payloads and can restore an imported unsupported method", () => {
  const events = loadModule("../components/webhooks/WebhookEventEditor.tsx", { "~/lib/webhook-editor-draft": draftContract });
  const operation = sourceDocument().webhooks["Issue:Created"].post;
  const document = { openapi: "3.1.0", webhooks: { "invoice.deleted": { delete: operation } } };
  let draft = draftContract.readWebhookDraft(JSON.stringify(document));
  // Rerendering reuses the actual callbacks and stable row identity, like the parent editor.
  const controls = () => eventFormControls(createElement(events.WebhookEventEditor, { draft, onChange(next) { draft = next; } }));
  const rowID = draft.events[0].id;
  controls().find((element) => element.type === "select").props.onChange({ target: { value: "get" } });
  assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["invoice.deleted"], { get: operation });
  controls().find((element) => element.type === "select").props.onChange({ target: { value: "post" } });
  assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(draft)).webhooks["invoice.deleted"], { post: operation });
  const restore = controls().find((element) => element.type === "button" && element.props.children?.[0] === "Restore original ");
  assert.ok(restore);
  restore.props.onClick();
  assert.equal(draft.events[0].id, rowID);
  assert.deepEqual(JSON.parse(draftContract.serializeWebhookDraft(draft)), document);
});

// The established API client must remain the only write route for either editor input mode.
test("editor writes use existing plan/apply and never workspace webhook setup", () => {
  const hook = readFileSync(new URL("../components/webhooks/useWebhookEditor.ts", import.meta.url), "utf8");
  const api = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
  assert.match(hook, /api\.integrations\.planImport/);
  assert.match(hook, /api\.integrations\.applyImport\(plan\.plan_id, plan\.review_hash\)/);
  assert.match(api, /"\/integrations\/import\/plan"/);
  assert.match(api, /"\/integrations\/import\/apply"/);
  assert.doesNotMatch(hook, /api\.buckets|api\.webhook|webhook-config\/|localStorage|sessionStorage/);
});

// Exercise the actual shared transport so route and reviewed-receipt parity is behavioural, not only textual.
test("actual import client sends both webhook inputs to the existing endpoints", async () => {
  const calls = [];
  const originalFetch = globalThis.fetch;
  // Synthetic HTTP responses isolate browser transport from every local or live workspace.
  globalThis.fetch = async (url, init) => { calls.push({ url, init }); return { ok: true, status: 200, json: async () => ({ plan_id: "plan", review_hash: "review" }) }; };
  try {
    const { api } = loadModule("./api.ts", {
      "./session": { getCSRFToken: () => "synthetic-csrf", purgeLegacyBrowserCredential() {} },
      "./browser-request": browserRequest, "./graphql-response": { unwrapGraphQLResponse: (response) => response.data }, "./workspace-notification": {}, "./authorization-error": apiErrors,
    });
    const input = importContract.webhookImportInput({ name: "Test", slug: "test" }, "1", { service_id: "service", service_version_id: "version", revision: 1 }, "source");
    await api.integrations.planImport(input);
    await api.integrations.planImport({ ...input, source_content: "uploaded YAML" });
    await api.integrations.applyImport("plan", "review");
    await api.integrations.importStatus("plan");
    await api.graphql("query Fresh { serviceWebhookEditor { revision } }", { service_id: "service" }, { headers: { "Cache-Control": "no-cache" } });
    assert.deepEqual(calls.map(({ url }) => url), ["/integrations/import/plan", "/integrations/import/plan", "/integrations/import/apply", "/integrations/import/operations/plan", "/graphql"]);
    assert.deepEqual(JSON.parse(calls[2].init.body), { plan_id: "plan", review_hash: "review" });
    assert.equal(calls[0].init.headers.get("X-Fused-CSRF"), "synthetic-csrf");
    assert.equal(calls[4].init.headers.get("Cache-Control"), "no-cache");
    assert.equal(calls[4].init.headers.get("X-Fused-CSRF"), "synthetic-csrf");
    assert.equal(calls[4].init.method, "POST");
  } finally { globalThis.fetch = originalFetch; }
});

// Minimal hook slots let tests drive the production controller without introducing another implementation.
function stateHarness() {
  const slots = [];
  const effects = [];
  let cursor = 0;
  let effectCursor = 0;
  return {
    reset() { cursor = 0; effectCursor = 0; },
    react: {
      // Each state slot persists exactly as React does across a controller rerender.
      useState(initial) {
        const index = cursor++;
        // Lazy initialization must run only on the first render.
        if (index === slots.length) slots.push(typeof initial === "function" ? initial() : initial);
        return [slots[index], (value) => { slots[index] = value; }];
      },
      // Stable dependencies must not issue another GraphQL read after a local form edit.
      useEffect(callback, dependencies) {
        const index = effectCursor++;
        // Only changed effect dependencies rerun their side-effect boundary.
        if (effects[index]?.every((value, position) => value === dependencies[position])) return;
        effects[index] = dependencies;
        callback();
      },
    },
  };
}

// Wire only synthetic IO while retaining the actual draft, planning, and recovery controller code.
async function editorHarness(overrides = {}) {
  const baseline = { service_id: "service", service_version_id: "version-id", revision: 7, source_content: overrides.source ?? JSON.stringify(sourceDocument()) };
  const plan = { plan_id: "reviewed-plan", review_hash: "reviewed-hash", service_id: "service", target_version: "v1", action: "update_version", is_new_service: false, expected_target: { service_id: baseline.service_id, service_version_id: baseline.service_version_id, revision: baseline.revision }, webhook_draft: { source_content: baseline.source_content } };
  const calls = [];
  let saved = 0;
  const state = stateHarness();
  const { useWebhookEditor } = loadModule("../components/webhooks/useWebhookEditor.ts", {
    react: state.react,
    "~/lib/webhook-editor-draft": draftContract,
    "~/lib/webhook-editor-import": importContract,
    "~/lib/authorization-error": apiErrors,
    "~/lib/api": { api: {
      // One complete selected-version read replaces any per-event query loop.
      graphql: async (query, variables, options) => {
        calls.push({ kind: "read", variables, options });
        return { serviceWebhookEditor: await overrides.read?.(baseline, calls.filter(({ kind }) => kind === "read").length) ?? baseline };
      },
      integrations: {
        // Echo the actual request guard so accepting a fresh baseline can be asserted through the next plan.
        planImport: async (input) => { calls.push({ kind: "plan", input }); return await overrides.plan?.(input, plan) ?? { ...plan, expected_target: input.expected_target }; },
        // Awaiting the test boundary also covers an apply that has not returned a known outcome yet.
        applyImport: async (...args) => { calls.push({ kind: "apply", args }); await overrides.apply?.(); return { status: "applied", service_id: "service", service_version_id: "version-id", version: "v1", revision: 8, is_new_service: false }; },
        importStatus: async (operation) => { calls.push({ kind: "status", operation }); return overrides.status; },
      },
    } },
  });
  // Rerenders pick up pending state without manufacturing new IO calls.
  function render() { state.reset(); return useWebhookEditor({ id: "service", name: "Test", slug: "test" }, "v1", () => { saved++; }); }
  render();
  await new Promise((resolve) => setImmediate(resolve));
  return { render, calls, baseline, plan, saved: () => saved };
}

// Source edits after review cannot accidentally apply the old source hash.
test("controller invalidates review after edits and applies only the exact reviewed receipt", async () => {
  const harness = await editorHarness();
  let editor = harness.render();
  editor.change({ ...editor.draft, events: [] });
  editor = harness.render();
  await editor.review();
  editor = harness.render();
  assert.equal(editor.plan.plan_id, "reviewed-plan");
  editor.markDirty();
  editor = harness.render();
  await editor.save();
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 0);
  await editor.review();
  await harness.render().save();
  assert.deepEqual(harness.calls.find(({ kind }) => kind === "apply").args, ["reviewed-plan", "reviewed-hash"]);
  assert.equal(harness.saved(), 1);
  assert.equal(harness.calls.filter(({ kind }) => kind === "read").length, 1);
});

// Upload preview and replacement remain two explicit actions, and replacement requires fresh planning.
test("controller previews files without saving or replacing the draft automatically", async () => {
  const harness = await editorHarness();
  const initial = harness.render().draft;
  await harness.render().importFile(new File(["openapi: 3.1.0"], "webhooks.yaml"));
  let editor = harness.render();
  assert.equal(editor.draft, initial);
  assert.ok(editor.pendingFile);
  assert.equal(editor.plan, null);
  editor.acceptFile();
  editor = harness.render();
  assert.equal(editor.pendingFile, null);
  assert.equal(editor.plan, null);
  assert.equal(editor.dirty, true);
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 0);
  await editor.review();
  assert.equal(harness.calls.filter(({ kind }) => kind === "plan").length, 2);
});

// An interrupted write is recovered by a read, with no hidden retry of the mutation.
test("controller retains draft on timeout and checks durable status before another save", async () => {
  const harness = await editorHarness({ apply: () => { throw new Error("Interrupted transport"); }, status: { status: "applied", commit_state: "committed", service_id: "service", service_version_id: "version-id", version: "v1", revision: 8 } });
  let editor = harness.render();
  editor.change(editor.draft);
  await harness.render().review();
  await harness.render().save();
  editor = harness.render();
  assert.equal(editor.uncertain, true);
  assert.ok(editor.draft);
  await editor.save();
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 1);
  await editor.checkStatus();
  assert.equal(harness.saved(), 1);
  assert.equal(harness.calls.find(({ kind }) => kind === "status").operation, "reviewed-plan");
});

// Fixtures use the real structured error so conflict recognition never depends on wording or HTTP status alone.
function targetChangedError(code = "IMPORT_TARGET_CHANGED", commitState = "not_committed") {
  return new APIRequestError(409, { error: "The selected version changed.", code, phase: "registry_apply", commit_state: commitState, operation_id: "reviewed-plan", request_id: "request-id", remediation: "Keep your unsaved draft for reference.", recovery: "fused-cli import plan --help" });
}

// A concurrent owner's event makes accidental replay of the old full source observable in subsequent plan input.
function newerWebhookSource(baseline) {
  return { ...baseline, revision: baseline.revision + 1, source_content: JSON.stringify({ ...sourceDocument(), webhooks: { "Remote:Created": { post: { summary: "Another owner's addition", responses: { "200": { description: "Accepted" } } } } } }) };
}

// Common setup reaches a real stale apply without duplicating the production conflict state machine.
async function conflictedEditor(overrides = {}) {
  const harness = await editorHarness({ apply: () => { throw targetChangedError(); }, ...overrides });
  const initial = harness.render();
  initial.change({ ...initial.draft, events: initial.draft.events.map((event) => ({ ...event, description: "My unsaved description" })) });
  await harness.render().review();
  await harness.render().save();
  return harness;
}

// A stale apply cannot be retried or silently rebased; the owner must deliberately choose the fresh baseline.
test("stale apply keeps the exact draft and requires explicit latest-baseline reconciliation", async () => {
  const source = '{"openapi":"3.1.0","components":{"schemas":{"Limit":{"maximum":9007199254740993,"multipleOf":1e-400}}},"webhooks":{"Local:Created":{"post":{"summary":"Before"}}}}';
  const harness = await conflictedEditor({ source, read: (baseline, count) => count === 1 ? baseline : newerWebhookSource(baseline) });
  let editor = harness.render();
  const original = editor.draft;
  assert.equal(editor.staleTarget, true);
  assert.equal(editor.uncertain, false);
  assert.equal(editor.plan, null);
  assert.equal(editor.dirty, true);
  const callCount = harness.calls.length;
  await editor.review(); await editor.save();
  assert.equal(harness.calls.length, callCount);
  await editor.loadLatest();
  editor = harness.render();
  assert.equal(editor.draft, original);
  assert.equal(editor.baselineRevision, 7);
  assert.equal(editor.latest.source.revision, 8);
  assert.match(editor.latest.referenceSource, /"maximum":9007199254740993/);
  assert.match(editor.latest.referenceSource, /"multipleOf":1e-400/);
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 1);
  assert.deepEqual(harness.calls.filter(({ kind }) => kind === "read").map(({ options }) => options.headers), [{ "Cache-Control": "no-cache" }, { "Cache-Control": "no-cache" }]);
  editor.startFromLatest();
  editor = harness.render();
  assert.equal(editor.staleTarget, false);
  assert.equal(editor.baselineRevision, 8);
  assert.equal(editor.dirty, false);
  assert.equal(editor.plan, null);
  assert.equal(editor.draft.events[0].name, "Remote:Created");
  assert.match(editor.retainedSource, /My unsaved description/);
  assert.match(editor.retainedSource, /9007199254740993/);
  editor.change({ ...editor.draft, events: [...editor.draft.events, { ...draftContract.newWebhookEvent(), name: "Local:Reapplied" }] });
  await harness.render().review();
  const reviewed = harness.calls.filter(({ kind }) => kind === "plan").at(-1).input;
  assert.equal(reviewed.expected_target.revision, 8);
  assert.equal(reviewed.expected_target.service_version_id, "version-id");
  assert.match(reviewed.source_content, /Remote:Created/);
  assert.match(reviewed.source_content, /Local:Reapplied/);
  assert.doesNotMatch(reviewed.source_content, /Local:Created/);
});

// Planner conflicts have a lowercase code but must present exactly the same safe recovery as apply conflicts.
test("a stale re-review retains the draft and cannot repeat the rejected baseline", async () => {
  const harness = await editorHarness({ plan: () => { throw targetChangedError("import_target_changed"); } });
  const original = harness.render().draft;
  harness.render().change(original);
  await harness.render().review();
  const editor = harness.render();
  assert.equal(editor.staleTarget, true);
  assert.equal(editor.draft, original);
  assert.equal(editor.plan, null);
  assert.match(editor.error, /import_target_changed/);
  await editor.review();
  assert.equal(harness.calls.filter(({ kind }) => kind === "plan").length, 1);
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 0);
});

// A failed reload must not replace either the original draft or its revision, even after a valid comparison existed.
test("recovery read failure and cached or recreated targets leave the original draft untouched", async () => {
  let unavailable = false;
  const harness = await conflictedEditor({ read: (baseline, count) => {
    // A later outage must leave the already loaded comparison available without changing either baseline.
    if (unavailable) throw new Error("Read unavailable");
    return count === 1 ? baseline : newerWebhookSource(baseline);
  } });
  const original = harness.render().draft;
  await harness.render().loadLatest();
  const candidate = harness.render().latest;
  unavailable = true;
  await harness.render().loadLatest();
  // A separate fixture also checks failure before any comparison candidate has been loaded.
  const failing = await conflictedEditor({ read: (baseline, count) => { if (count > 1) throw new Error("Read unavailable"); return baseline; } });
  const failedOriginal = failing.render().draft;
  await failing.render().loadLatest();
  assert.equal(failing.render().draft, failedOriginal);
  assert.equal(failing.render().baselineRevision, 7);
  assert.equal(failing.render().latest, null);
  assert.match(failing.render().error, /Read unavailable/);
  assert.equal(harness.render().draft, original);
  assert.equal(harness.render().latest, candidate);
  assert.throws(() => importContract.assertWebhookRecoverySource(harness.baseline, harness.baseline), /newer revision/);
  assert.throws(() => importContract.assertWebhookRecoverySource({ ...newerWebhookSource(harness.baseline), service_version_id: "recreated" }, harness.baseline), /exact service version/);
  assert.throws(() => importContract.assertWebhookRecoverySource({ ...newerWebhookSource(harness.baseline), service_id: "another" }, harness.baseline), /exact service version/);
});

// An unknown result locks every controller entrypoint, including stale-looking errors, until durable status proves rejection.
test("uncertain apply only permits status and a confirmed stale rejection then unlocks reconciliation", async () => {
  const status = { status: "pending", phase: "registry_apply", commit_state: "unknown", operation_id: "reviewed-plan" };
  const harness = await conflictedEditor({ apply: () => { throw targetChangedError("IMPORT_TARGET_CHANGED", "unknown"); }, status });
  let editor = harness.render();
  const original = editor.draft;
  const receipt = editor.plan;
  assert.equal(editor.uncertain, true);
  assert.equal(editor.staleTarget, false);
  const count = harness.calls.length;
  await editor.loadLatest(); await editor.review(); await editor.importFile(new File(["{}"], "webhooks.json")); await editor.save();
  editor.change({ ...editor.draft, events: [] }); editor.markDirty(); editor.startFromLatest();
  assert.equal(harness.calls.length, count);
  assert.equal(harness.render().draft, original);
  assert.equal(harness.render().plan, receipt);
  await editor.checkStatus();
  assert.equal(harness.render().uncertain, true);
  Object.assign(status, { status: "failed", commit_state: "not_committed", code: "IMPORT_TARGET_CHANGED", recovery: "fused-cli import plan --help", guidance: "Keep your draft for reference." });
  await harness.render().checkStatus();
  editor = harness.render();
  assert.equal(editor.uncertain, false);
  assert.equal(editor.staleTarget, true);
  assert.equal(editor.plan, null);
  assert.equal(editor.draft, original);
  assert.match(editor.error, /IMPORT_TARGET_CHANGED.*registry_apply.*not_committed.*reviewed-plan/);
  assert.doesNotMatch(editor.error, /fused-cli/);
  assert.equal(harness.calls.filter(({ kind }) => kind === "apply").length, 1);
});

// Tailored browser help must preserve stable diagnostics without echoing the same CLI command twice.
test("webhook error rendering keeps diagnostics and removes only conflict CLI advice", () => {
  const rendered = importContract.webhookEditorError(targetChangedError());
  assert.match(rendered, /The selected version changed/);
  assert.match(rendered, /Keep your unsaved draft/);
  assert.match(rendered, /IMPORT_TARGET_CHANGED.*registry_apply.*not_committed.*reviewed-plan.*request-id/);
  assert.doesNotMatch(rendered, /fused-cli/);
  const other = new APIRequestError(503, { error: "Try status.", code: "IMPORT_UNAVAILABLE", recovery: "fused-cli import status operation" });
  assert.equal(importContract.webhookEditorError(other).match(/fused-cli/g).length, 1);
});

// The actual drawer must expose intentional reconciliation and protect a retained copy even before further edits.
test("recovery drawer renders read-only source and blocks accidental discard after starting latest", async () => {
  const harness = await conflictedEditor({ read: (baseline, count) => count === 1 ? baseline : newerWebhookSource(baseline) });
  await harness.render().loadLatest();
  let editor = harness.render();
  let blocked = false;
  let beforeUnload;
  const { WebhookDefinitionEditor } = loadModule("../components/webhooks/WebhookDefinitionEditor.tsx", {
    "@remix-run/react": { useBlocker: (value) => { blocked = value; return { state: "unblocked" }; }, useBeforeUnload: (callback) => { beforeUnload = callback; } },
    "./useWebhookEditor": { useWebhookEditor: () => editor },
    "./WebhookEventEditor": { WebhookEventEditor: () => null },
    "./WebhookSettingsEditor": { WebhookSettingsEditor: () => null },
  });
  // Rendering the production drawer exercises its explicit buttons and protection state without a browser mutation.
  const render = () => renderToStaticMarkup(createElement(WebhookDefinitionEditor, { service: { id: "service", name: "Test" }, version: "v1", onClose() {}, onSaved() {} }));
  let markup = render();
  assert.match(markup, /Load latest for comparison/);
  assert.match(markup, /Start from latest; keep my draft for reference/);
  assert.match(markup, /textarea[^>]*readonly/);
  assert.match(markup, /fieldset[^>]*disabled/);
  assert.match(markup, /overwrite other people/);
  editor.startFromLatest();
  editor = harness.render();
  markup = render();
  assert.equal(editor.dirty, false);
  assert.equal(blocked, true);
  assert.match(markup, /Your retained draft before reloading/);
  assert.doesNotMatch(markup, /Start from latest; keep my draft for reference/);
  let prevented = false;
  beforeUnload({ preventDefault() { prevented = true; }, returnValue: undefined });
  assert.equal(prevented, true);
});

// Unsafe integers, fractions, overflow and underflow remain exact tokens instead of rounded JS numbers.
test("native raw JSON preserves every numeric source token without arithmetic or exponent expansion", () => {
  for (const value of ["9007199254740993", "0.123456789012345678901", "1.0000000000000001", "1e-400", "1e9999999", "9.007199254740993e15"]) {
    assert.equal(JSON.stringify(parseWebhookEditorJSON(`{"maximum":${value}}`)), `{"maximum":${value}}`);
    const source = `{"webhooks":{},"components":{"schemas":{"Event":{"maximum":${value}}}}}`;
    assert.equal(draftContract.serializeWebhookDraft(draftContract.readWebhookDraft(source)), source);
  }
});

// Even harmless notation changes are unnecessary because native raw JSON preserves the original spelling.
test("native raw JSON preserves decimal/exponent spellings and negative zero", () => {
  for (const value of ["0.1", "1.50", "150e-2", "1e3", "-0", "9007199254740992", "2.5e-10"]) {
    assert.equal(JSON.stringify(parseWebhookEditorJSON(value)), value);
  }
});

// Optional schema/example text uses exactly the same native preservation adapter as server draft hydration.
test("payload edits preserve exact schema constraints and example values", () => {
  const draft = draftContract.readWebhookDraft(JSON.stringify(sourceDocument()));
  draft.events[0].schemaText = '{"maximum":9007199254740993}';
  assert.match(draftContract.serializeWebhookDraft(draft), /"maximum":9007199254740993/);
  draft.events[0].schemaText = undefined;
  draft.events[0].exampleText = '{"amount":0.123456789012345678901}';
  assert.match(draftContract.serializeWebhookDraft(draft), /"amount":0\.123456789012345678901/);
  draft.events[0].exampleText = " ".repeat(draftContract.webhookEditorMaxBytes + 1);
  assert.throws(() => draftContract.serializeWebhookDraft(draft), /4 MiB/);
});

// Older browsers must not silently use a lossy fallback when the native exact-token API is absent.
test("numeric-containing drafts fail closed without native raw JSON", () => {
  const original = JSON.rawJSON;
  JSON.rawJSON = undefined;
  try { assert.throws(() => parseWebhookEditorJSON('{"maximum":9007199254740993}'), /cannot preserve JSON numbers exactly/); }
  finally { JSON.rawJSON = original; }
});
