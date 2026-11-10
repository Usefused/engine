import assert from "node:assert/strict";
import test from "node:test";
import { readFileSync } from "node:fs";
import ts from "typescript";
import * as detail from "./webhook-detail.ts";
import { webhookDetailPath } from "./webhook-discovery-contract.ts";

/** Exercises the production query adapter through a synthetic transport without a live Engine. */
function adapter(api) {
  const source = readFileSync(new URL("./webhook-detail-api.ts", import.meta.url), "utf8");
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 } }).outputText;
  const exports = {};
  /** Keeps validation real while restricting network access to this test's recorded transport. */
  function require(name) {
    // Reporting tests must never read or mutate a real workspace.
    if (name === "./api") return { api };
    // The actual date and identity validation remains part of the adapter under test.
    if (name === "./webhook-detail") return detail;
    throw new Error(`Unexpected dependency ${name}`);
  }
  new Function("require", "exports", code)(require, exports);
  return exports;
}

// Both independent grants must cover this resource; null/loading access never authorizes Activity.
test("registration Activity permission matrix and direct URLs remain scoped", () => {
  const base = { workspace_id: "workspace", grants: [] };
  const service = { permission: "service.read", resource_type: "SERVICE", resource_id: "stripe" };
  const audit = { permission: "audit.read", resource_type: "WORKSPACE", resource_id: "workspace" };
  for (const grants of [[], [service], [audit], [{ ...service, resource_id: "other" }, audit]]) {
    assert.equal(detail.canReadWebhookActivity({ ...base, grants }, "stripe"), false);
    assert.deepEqual(detail.webhookDetailTabs(false).map((tab) => tab.value), ["overview"]);
    assert.equal(detail.selectedWebhookTab("events", false), "overview");
    assert.equal(detail.selectedWebhookTab("analytics", false), "overview");
  }
  assert.equal(detail.canReadWebhookActivity(null, "stripe"), false);
  assert.equal(detail.canReadWebhookActivity({ ...base, grants: [service, audit] }, "stripe"), true);
  assert.deepEqual(detail.webhookDetailTabs(true).map((tab) => tab.value), ["overview", "analytics", "events"]);
  assert.equal(detail.selectedWebhookTab("events", true), "events");
  assert.equal(detail.selectedWebhookTab("analytics", true), "analytics");
  assert.equal(detail.selectedWebhookTab("unknown", true), "overview");
});

// Receiving-route identity and inclusive UTC calendar bounds survive navigation without ambiguous searches.
test("detail links and dates retain exact identities and inclusive periods", () => {
  assert.equal(webhookDetailPath({ service_id: "service/id", slug: "route?other" }), "/integrations/webhooks/service%2Fid/route%3Fother");
  assert.equal(detail.webhookDateBound(""), null);
  assert.equal(detail.webhookDateBound("2026-11-10"), "2026-11-10T00:00:00.000Z");
  assert.equal(detail.webhookDateBound("2026-11-10", true), "2026-11-10T23:59:59.999Z");
  assert.throws(() => detail.webhookDateBound("2026-11-10?secret"), /valid date/);
});

// Actual signature rejections have an untrusted event type; their fallback appears once in the shared filter.
test("rejected events do not duplicate UNKNOWN and paging retains the selected event", () => {
  assert.deepEqual(detail.webhookEventNameOptions(["UNKNOWN", "", "invoice.paid", "invoice.paid"], "UNKNOWN"), ["invoice.paid"]);
  assert.deepEqual(detail.webhookEventNameOptions([], "invoice.paid"), ["invoice.paid"]);
  assert.deepEqual(detail.webhookEventNameOptions(["UNKNOWN"], ""), []);
});

// SQL receives exact scope, outcome, and pagination; no mixed-registration page is filtered in the browser.
test("details, summaries, failures and pages send exact scope to Engine", async () => {
  const calls = [];
  const client = adapter({
    /** Records actual query variables and supplies deterministic response shapes. */
    mcpGraphql: async (query, variables, options) => {
      calls.push({ query, variables, options });
      return { workspaceWebhook: { id: "registration", slug: "route" }, webhookAnalytics: { total_ingested: 45 }, webhookEvents: { items: [], total: 45 } };
    },
  });
  await client.webhookDetails("stripe", "route");
  const input = { serviceId: "stripe", registrationId: "registration", startDate: "2026-11-01", endDate: "2026-11-10" };
  await client.webhookRegistrationAnalytics(input);
  await client.webhookRegistrationEvents({ ...input, status: "failed", eventName: "invoice.paid", limit: 20, offset: 40 });
  assert.deepEqual(calls[0].variables, { serviceId: "stripe", slug: "route" });
  assert.deepEqual(calls[1].variables, { serviceId: "stripe", registrationId: "registration", startDate: "2026-11-01T00:00:00.000Z", endDate: "2026-11-10T23:59:59.999Z" });
  assert.deepEqual(calls[2].variables, { ...calls[1].variables, status: "failed", eventName: "invoice.paid", limit: 20, offset: 40 });
  assert.match(calls[1].query, /registration_id: \$registrationId/);
  assert.match(calls[2].query, /registration_id: \$registrationId/);
  assert.match(calls[2].query, /error_reason/);
  assert.ok(calls.every((call) => call.options.signal instanceof AbortSignal));
  await assert.rejects(client.webhookRegistrationEvents({ serviceId: "stripe", registrationId: "" }), /exact webhook/);
  assert.equal(calls.length, 3);
});

// Cancellation reaches the request signal and transport errors remain explicit instead of fabricating empty activity.
test("navigation cancels reports and unavailable data propagates", async () => {
  const controller = new AbortController();
  const client = adapter({
    /** Mirrors fetch cancellation entirely within the test process. */
    mcpGraphql: async (_query, _variables, { signal }) => new Promise((_resolve, reject) => {
      signal.addEventListener("abort", () => reject(signal.reason), { once: true });
    }),
  });
  const pending = client.webhookRegistrationEvents({ serviceId: "stripe", registrationId: "registration" }, controller.signal);
  controller.abort();
  await assert.rejects(pending, { name: "AbortError" });
  const failed = adapter({
    /** An unavailable Engine must never become zero received events. */
    mcpGraphql: async () => { throw new Error("Synthetic outage"); },
  });
  await assert.rejects(failed.webhookRegistrationAnalytics({ serviceId: "stripe", registrationId: "registration" }), /Synthetic outage/);
});
