import assert from "node:assert/strict";
import test from "node:test";
import {
  buildWebhookAttachmentCoverage,
  mergeWebhookFocusServices,
  serviceWebhookCoverageNotice,
  uncoveredWebhookEventServices,
  webhookCoverageMismatchMessage,
} from "./unified-app-webhook-coverage.ts";

// Exact label filtering prevents sibling webhook registrations returned by substring search from spoofing coverage.
test("buildWebhookAttachmentCoverage isolates exact labels and deduplicates services", () => {
  const coverage = buildWebhookAttachmentCoverage("margin-billing", [
    { label: "margin-billing", service_id: "stripe-id", service_name: "Stripe" },
    { label: "margin-billing", service_id: "stripe-id", service_name: "Stripe" },
    { label: "margin-billing-v2", service_id: "openai-id", service_name: "OpenAI" },
    { label: "margin-billing", service_id: "jira-id", service_name: "Jira" },
  ]);
  assert.equal(coverage.attachment, "margin-billing");
  assert.deepEqual([...coverage.serviceIds], ["stripe-id", "jira-id"]);
  assert.deepEqual(coverage.serviceNames, ["Stripe", "Jira"]);
  assert.deepEqual(coverage.services, [
    { serviceId: "stripe-id", serviceName: "Stripe" },
    { serviceId: "jira-id", serviceName: "Jira" },
  ]);
});

// Operation-only services never require webhook registration, while uncovered event services are reported only when readable.
test("uncoveredWebhookEventServices reports only readable event-selecting services missing from the bundle", () => {
  const coverage = buildWebhookAttachmentCoverage("margin-billing", [
    { label: "margin-billing", service_id: "stripe-id", service_name: "Stripe" },
  ]);
  const draft = {
    name: "margin-ai-generation",
    description: "",
    source: "export default buildUnifiedApp({});",
    webhookAttachment: "margin-billing",
    services: {
      OpenAI: { service_id: "openai-id", service_version_id: "openai-v1", version: "2.3.0", operations: ["createResponse"], webhooks: ["agent.session.created"] },
      Stripe: { service_id: "stripe-id", service_version_id: "stripe-v1", version: "1.0.0", operations: [], webhooks: ["invoice.paid"] },
      Slack: { service_id: "slack-id", service_version_id: "slack-v1", version: "1.0.0", operations: ["postMessage"], webhooks: [] },
      Hidden: { service_id: "hidden-id", service_version_id: "hidden-v1", version: "1.0.0", operations: [], webhooks: [] },
    },
    serviceSettings: {
      Hidden: { service_id: "hidden-id", config: { webhooks_select_all: true } },
    },
  };
  /** Grants read access for every service except Hidden to verify fail-safe RBAC behavior. */
  const canRead = (serviceId) => serviceId !== "hidden-id";
  assert.deepEqual(uncoveredWebhookEventServices(draft, coverage, canRead), ["OpenAI"]);
  // When Hidden becomes readable, webhooks_select_all is treated as selecting events just like explicit event names.
  assert.deepEqual(uncoveredWebhookEventServices(draft, coverage, () => true), ["OpenAI", "Hidden"]);
  // Stale coverage for a different attachment label must not misdiagnose the current draft.
  assert.deepEqual(uncoveredWebhookEventServices({ ...draft, webhookAttachment: "other-hook" }, coverage, () => true), []);
});

// Messages name both the registered and missing providers so authors immediately know how to resolve the mismatch.
test("coverage messages explain registered services and handle empty attachments", () => {
  assert.equal(webhookCoverageMismatchMessage("margin-billing", [], ["Stripe"]), "");
  assert.match(
    webhookCoverageMismatchMessage("margin-billing", ["OpenAI"], ["Stripe"]),
    /Webhook "margin-billing" registers Stripe, not OpenAI/,
  );
  assert.match(
    webhookCoverageMismatchMessage("missing-hook", ["OpenAI"], []),
    /Webhook "missing-hook" does not register OpenAI/,
  );
  const coverage = buildWebhookAttachmentCoverage("margin-billing", [
    { label: "margin-billing", service_id: "stripe-id", service_name: "Stripe" },
  ]);
  assert.equal(serviceWebhookCoverageNotice("stripe-id", "Stripe", coverage, true), "");
  assert.equal(serviceWebhookCoverageNotice("openai-id", "OpenAI", coverage, false), "");
  assert.match(
    serviceWebhookCoverageNotice("openai-id", "OpenAI", coverage, true),
    /Attached webhook "margin-billing" registers Stripe, not OpenAI/,
  );
});

// Explicit registration choices stay first while additional services from multi-service bundles append without duplicates.
test("mergeWebhookFocusServices preserves explicit service priority and deduplicates bundle members", () => {
  const coverage = buildWebhookAttachmentCoverage("team-events", [
    { label: "team-events", service_id: "jira-id", service_name: "Jira" },
    { label: "team-events", service_id: "stripe-id", service_name: "Stripe" },
  ]);
  assert.deepEqual(
    mergeWebhookFocusServices({ serviceId: "stripe-id", serviceName: "Stripe" }, coverage),
    [
      { serviceId: "stripe-id", serviceName: "Stripe" },
      { serviceId: "jira-id", serviceName: "Jira" },
    ],
  );
  assert.deepEqual(mergeWebhookFocusServices(null, coverage), [
    { serviceId: "jira-id", serviceName: "Jira" },
    { serviceId: "stripe-id", serviceName: "Stripe" },
  ]);
});
