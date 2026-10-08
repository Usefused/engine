import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import ts from "typescript";
import * as importContract from "./webhook-editor-import.ts";

const require = createRequire(import.meta.url);

// Exercise production controller decisions with persistent state and synthetic network boundaries.
function harness(overrides = {}) {
  const slots = [];
  let cursor = 0;
  let saves = 0;
  const calls = [];
  const plan = { plan_id: "review-id", review_hash: "review-hash", service_id: "service", target_version: "v1", target_type: "endpoints", action: "update_version", is_new_service: false };
  const modules = {
    react: {
      // Rerenders keep state and receipt identity while exposing the latest callbacks.
      useState(initial) {
        const index = cursor++;
        // Only the first render initializes a hook slot.
        if (index === slots.length) slots.push(initial);
        // State updates are visible on the next controller render, as in React.
        return [slots[index], (value) => { slots[index] = value; }];
      },
    },
    "@remix-run/react": {
      // Navigation is outside these transport-focused tests.
      useBeforeUnload() {},
      // No synthetic navigation should interfere with review and recovery.
      useBlocker() { return { state: "unblocked" }; },
    },
    "~/lib/webhook-editor-import": importContract,
    "~/lib/api": { api: { integrations: {
      // Capture the real pinned request and optionally simulate a mismatched server receipt.
      async planImport(input) { calls.push({ kind: "plan", input }); return { ...plan, ...overrides.plan }; },
      // A network failure models an unknown commit, never a confirmed rollback.
      async applyImport(...args) {
        calls.push({ kind: "apply", args });
        // This scenario deliberately interrupts after a write might have committed.
        if (overrides.failApply) throw new Error("Connection interrupted");
        return { status: "applied", service_id: "service", version: "v1", is_new_service: false };
      },
      // Recovery confirms the original operation without another apply call.
      async importStatus(id) { calls.push({ kind: "status", id }); return { status: "applied", commit_state: "committed", service_id: "service", version: "v1" }; },
    } } },
  };
  const source = readFileSync(new URL("../components/integration-details/ServiceEndpointImport.tsx", import.meta.url), "utf8");
  const output = ts.transpileModule(source, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  const module = { exports: {} };
  // IO substitutions take precedence; the real JSX runtime remains available to the module.
  new Function("require", "module", "exports", output)((name) => modules[name] ?? require(name), module, module.exports);
  // Each controller render retains the same service, selected version, and hooks.
  function render() {
    cursor = 0;
    return module.exports.useEndpointImport({
      service: { id: "service", name: "Example", slug: "example" }, version: "v1",
      // Dismissal is unrelated to these write-boundary tests.
      onClose() {},
      // Only confirmed commits may invoke this completion callback.
      onSaved() { saves++; },
    });
  }
  render().setSource('{"openapi":"3.1.0"}');
  // Expose completion count without duplicating controller state.
  return { render, calls, saves: () => saves };
}

// This synthetic form event exercises the same review entry point as the browser.
function submitEvent() { return { preventDefault() {} }; }

// An existing-service action must never become creation or target another type/version.
test("endpoint import rejects a mismatched reviewed destination", async () => {
  for (const plan of [{ service_id: "other" }, { target_version: "v2" }, { is_new_service: true }, { target_type: "webhooks" }]) {
    const fixture = harness({ plan });
    await fixture.render().review(submitEvent());
    assert.equal(fixture.render().plan, null);
    assert.match(fixture.render().error, /does not target/);
    await fixture.render().apply();
    assert.equal(fixture.calls.length, 1);
  }
});

// The review pins destination fields while apply sends only the exact opaque receipt.
test("endpoint import applies the exact reviewed receipt", async () => {
  const fixture = harness();
  await fixture.render().review(submitEvent());
  assert.deepEqual(fixture.calls[0].input, { name: "Example", slug: "example", version: "v1", target_type: "endpoints", source_content: '{"openapi":"3.1.0"}' });
  await fixture.render().apply();
  assert.deepEqual(fixture.calls[1].args, ["review-id", "review-hash"]);
  assert.equal(fixture.saves(), 1);
});

// Unknown commits retain their receipt and cannot be retried through either review or apply.
test("endpoint import blocks duplicate writes until the original status resolves", async () => {
  const fixture = harness({ failApply: true });
  await fixture.render().review(submitEvent());
  await fixture.render().apply();
  assert.equal(fixture.render().uncertain, true);
  await fixture.render().apply();
  await fixture.render().review(submitEvent());
  assert.equal(fixture.calls.length, 2);
  await fixture.render().checkStatus();
  assert.deepEqual(fixture.calls[2], { kind: "status", id: "review-id" });
  assert.equal(fixture.saves(), 1);
});
