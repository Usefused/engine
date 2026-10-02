import assert from "node:assert/strict";
import test from "node:test";
import { unifiedEditDraft, unifiedEditConfig, unifiedEditSelections, nextUnifiedVersion } from "./unified-app-edit.ts";

// Include credential routing and runtime settings so source editing cannot silently reset them.
function savedApp() {
  return {
    app_id: "app-v1", owner_team: "payments",
    config: {
      kind: "unified_app", name: "customer-app", version: "1.0.0", bucket: "production",
      source: "export default buildUnifiedApp({});", description: "Original description",
      bundle_digest: "sha256:old", source_path: "app.ts", timeout: 1000,
      webhook_attachment: "payment-events",
      services: { Stripe: { version: "v1", operations: ["GetCustomers"], webhooks: ["payment.succeeded"], auth: { name: "bearerAuth" }, bucket: "billing" } },
    },
    service_pins: [{ key: "Stripe", service_id: "stripe-id", service_version_id: "stripe-v1" }],
  };
}

// A successor retains immutable family identity and hidden settings while replacing only reviewed fields.
test("editing preserves family identity, provider overrides and exact pins", () => {
  const saved = savedApp();
  const before = structuredClone(saved);
  const draft = unifiedEditDraft(saved);
  assert.equal(draft.services.Stripe.service_version_id, "stripe-v1");
  draft.source += "\n// Updated implementation";
  draft.description = "Updated description";
  const config = unifiedEditConfig(saved, draft, "1.0.1");
  assert.equal(config.name, "customer-app");
  assert.equal(config.bucket, "production");
  assert.equal(config.timeout, 1000);
  assert.equal(config.source, draft.source);
  assert.equal(config.description, "Updated description");
  assert.deepEqual(config.services.Stripe.auth, { name: "bearerAuth" });
  assert.equal(config.services.Stripe.bucket, "billing");
  assert.equal(config.bundle_digest, undefined);
  assert.equal(config.source_path, undefined);
  assert.equal(config.webhook_attachment, "payment-events");
  assert.deepEqual(config.services.Stripe.webhooks, ["payment.succeeded"]);
  assert.deepEqual(saved, before);
});

// Canonical picker names cannot rename a provider referenced by the saved source or drop credential overrides.
test("picker hydration retains saved service aliases", () => {
  const saved = savedApp();
  const draft = unifiedEditDraft(saved);
  const selected = unifiedEditSelections(saved, { "@provider/stripe": { ...draft.services.Stripe, operations: ["GetCustomers", "PostCustomers"] } });
  assert.deepEqual(Object.keys(selected), ["Stripe"]);
  const config = unifiedEditConfig(saved, { ...draft, services: selected }, "1.0.1");
  assert.deepEqual(config.services.Stripe.operations, ["GetCustomers", "PostCustomers"]);
  assert.equal(config.services.Stripe.bucket, "billing");
});

// A service replacement cannot inherit secrets or routing intended for a different provider identity.
test("provider replacement does not inherit previous authentication settings", () => {
  const saved = savedApp();
  const draft = unifiedEditDraft(saved);
  draft.services.Stripe.service_id = "other-provider";
  assert.equal(unifiedEditConfig(saved, draft, "1.0.1").services.Stripe.auth, undefined);
});

// Removing the last event from a successor also removes its obsolete registration reference.
test("editing can remove webhook delivery", () => {
  const saved = savedApp();
  const draft = unifiedEditDraft(saved);
  draft.services.Stripe.webhooks = [];
  const config = unifiedEditConfig(saved, draft, "1.0.1");
  assert.equal(config.webhook_attachment, undefined);
  assert.deepEqual(config.services.Stripe.webhooks, []);
});

// Existing versions, absent source, and incomplete pins must fail before planning can create side effects.
test("invalid editing baselines and version reuse are rejected", () => {
  const saved = savedApp();
  const draft = unifiedEditDraft(saved);
  assert.throws(() => unifiedEditConfig(saved, draft, " 1.0.0 "), /new version/);
  assert.throws(() => unifiedEditConfig(saved, draft, ""), /new version/);
  assert.throws(() => unifiedEditDraft({ ...saved, service_pins: [] }), /identity/);
  assert.throws(() => unifiedEditDraft({ ...saved, service_pins: [...saved.service_pins, { ...saved.service_pins[0], key: "billing" }] }), /multiple aliases/);
  assert.throws(() => unifiedEditDraft({ ...saved, config: { ...saved.config, source: undefined } }), /no saved TypeScript/);
  assert.throws(() => unifiedEditDraft({ ...saved, config: { ...saved.config, kind: "sdk" } }), /no saved TypeScript/);
  assert.equal(nextUnifiedVersion("1.2.3"), "1.2.4");
  assert.equal(nextUnifiedVersion("release-one"), "");
});
