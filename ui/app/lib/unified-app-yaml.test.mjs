import assert from "node:assert/strict";
import test from "node:test";
import { unifiedEditorYAML, readUnifiedEditorYAML } from "./unified-app-yaml.ts";
import { unifiedConfig } from "./unified-app-contract.ts";
import { unifiedEditConfig, unifiedEditDraft } from "./unified-app-edit.ts";
import { applyAppAuthEdits } from "./app-service-auth.ts";
import { unifiedAuthOptions } from "./unified-app-auth.ts";

// Reference-only apps must preserve exact child versions from editable YAML through the plan request.
test("YAML admits and preserves Unified App dependencies without provider selections", () => {
  const draft = { ...unifiedEditDraft(fixture()), services: {}, serviceSettings: {} };
  const yaml = unifiedEditorYAML(draft, "parent", "1.0.0", "default") + 'unified_apps:\n  child:\n    name: Child\n    version: "1.0.0"\n';
  const parsed = readUnifiedEditorYAML(yaml, draft);
  const config = unifiedConfig(parsed.draft, parsed.name, parsed.version, parsed.bucket);
  assert.deepEqual(config.unified_apps, { child: { name: "Child", version: "1.0.0" } });
  assert.deepEqual(config.services, {});
  assert.match(unifiedEditorYAML(parsed.draft, parsed.name, parsed.version, parsed.bucket), /unified_apps:/);
});

// A saved app includes non-auth service settings that must survive changing only its authentication scheme.
function fixture() {
  const config = { apiVersion: "fused/v1", kind: "unified_app", name: "checkout", version: "1.0.0", bucket: "default", source: "export default buildUnifiedApp({});", services: { stripe: { version: "v1", operations: ["postCustomers"], bucket: "billing", auth: { type: "basic", name: "basicAuth" }, connect: { scopes: ["read"] }, injections: [{ location: "header", name: "X-Region", value: "${bucket.env.REGION}" }] } } };
  return { config, service_pins: [{ key: "stripe", service_id: "stripe-id", service_version_id: "stripe-v1" }] };
}

// Switching the YAML auth pair must affect the actual compile config, without touching TypeScript or routing.
test("YAML and form compile the same explicit bearer selection", () => {
  const saved = fixture(), draft = unifiedEditDraft(saved);
  const yaml = unifiedEditorYAML(draft, "checkout", "1.0.1", "default", saved).replace("type: basic", "type: bearer").replace("name: basicAuth", "name: bearerAuth");
  const parsed = readUnifiedEditorYAML(yaml, draft, saved);
  const config = unifiedEditConfig(saved, parsed.draft, parsed.version);
  assert.deepEqual(config.services.stripe.auth, { type: "bearer", name: "bearerAuth" });
  assert.equal(config.source, saved.config.source);
  assert.equal(config.services.stripe.bucket, "billing");
  assert.deepEqual(config.services.stripe.connect, { scopes: ["read"] });
  assert.deepEqual(config.services.stripe.injections, saved.config.services.stripe.injections);
  assert.equal(config.source_path, undefined);
  assert.doesNotMatch(yaml, /export default/);
});

// New apps carry identical auth fields into the CLI-compatible request, while removed auth remains removed.
test("new configs preserve auth and provider-default is an explicit removal", () => {
  const saved = fixture(), draft = unifiedEditDraft(saved);
  draft.serviceSettings.stripe.config.auth = { type: "bearer", name: "bearerAuth" };
  assert.deepEqual(unifiedConfig(draft, "checkout", "1.0.0", "default").services.stripe.auth, { type: "bearer", name: "bearerAuth" });
  delete draft.serviceSettings.stripe.config.auth;
  assert.equal(unifiedEditConfig(saved, draft, "1.0.1").services.stripe.auth, undefined);
  draft.serviceSettings.stripe.config.webhooks = ["customer.created"];
  draft.services.stripe.webhooks = [];
  assert.deepEqual(unifiedConfig(draft, "checkout", "1.0.0", "default").services.stripe.webhooks, []);
});

// Editable top-level settings and all-event routing must reach planning instead of being silently discarded.
test("YAML preserves edited optional settings and event registration requirements", () => {
  const draft = unifiedEditDraft(fixture());
  const yaml = unifiedEditorYAML(draft, "checkout", "1.0.1", "default") + "language: typescript\ngenerate: false\nwebhook_attachment: stripe-events\n";
  const parsed = readUnifiedEditorYAML(yaml.replace("webhooks: []", "webhooks: []\n    webhooks_select_all: true"), draft);
  const config = unifiedConfig(parsed.draft, parsed.name, parsed.version, parsed.bucket);
  assert.equal(config.language, "typescript");
  assert.equal(config.generate, false);
  assert.equal(config.webhook_attachment, "stripe-events");
  assert.equal(config.source_path, undefined);
  parsed.draft.webhookAttachment = "";
  assert.throws(() => unifiedConfig(parsed.draft, parsed.name, parsed.version, parsed.bucket), /webhook registration/);
});

// YAML is a config editor, not a way around immutable identity, provider grounding or secret storage.
test("invalid YAML and unreviewed identities fail before planning", () => {
  const saved = fixture(), draft = unifiedEditDraft(saved);
  const yaml = unifiedEditorYAML(draft, "checkout", "1.0.1", "default", saved);
  assert.throws(() => readUnifiedEditorYAML("name: [", draft, saved));
  assert.throws(() => readUnifiedEditorYAML(yaml.replace("version: v1", "version: v2"), draft, saved), /service picker/);
  assert.throws(() => readUnifiedEditorYAML(yaml.replace("name: checkout", "name: another"), draft, saved), /keeps its name/);
  assert.throws(() => readUnifiedEditorYAML(yaml.replace("source_path: app.ts", "source_path: ../secret.ts"), draft, saved), /source_path/);
  assert.throws(() => readUnifiedEditorYAML(yaml.replace("type: basic", "token: secret"), draft, saved), /Store credential/);
  assert.throws(() => readUnifiedEditorYAML(yaml + "\nversion: 2", draft, saved), /duplicated mapping/);
  assert.throws(() => readUnifiedEditorYAML(yaml + "\ncycle: &cycle [*cycle]", draft, saved), /aliases/);
});

// SDK and MCP composition cannot erase unrelated settings or carry auth across a provider replacement.
test("SDK MCP and Unified App use the same service auth shape", () => {
  for (const kind of ["sdk", "mcp", "unified_app"]) {
    const config = { ...fixture().config, kind };
    const auth = { type: "bearer", name: "bearerAuth" };
    const edits = { stripe: { service_id: "stripe-id", auth } };
    const changed = applyAppAuthEdits(config, edits, { stripe: "stripe-id" });
    assert.deepEqual(changed.services.stripe.auth, auth);
    assert.equal(changed.services.stripe.bucket, "billing");
    assert.deepEqual(config.services.stripe.auth, { type: "basic", name: "basicAuth" });
    assert.equal(applyAppAuthEdits(config, { stripe: { service_id: "stripe-id" } }, { stripe: "stripe-id" }).services.stripe.auth, undefined);
    assert.deepEqual(applyAppAuthEdits(config, edits, { stripe: "different-provider" }).services.stripe.auth, config.services.stripe.auth);
  }
});

// Two schemes of the same type remain separately selectable; HTTP/bearer is mapped to Fused's config spelling.
test("auth options preserve named identities", () => {
  const options = unifiedAuthOptions([{ type: "http", scheme: "bearer", name: "bearerAuth" }, { type: "bearer", name: "adminBearer" }, { type: "http", scheme: "basic", name: "basicAuth" }]);
  assert.deepEqual(options.map(({ type, name }) => ({ type, name })), [{ type: "bearer", name: "bearerAuth" }, { type: "bearer", name: "adminBearer" }, { type: "basic", name: "basicAuth" }]);
  assert.notEqual(options[0].key, options[1].key);
});
