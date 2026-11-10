import assert from "node:assert/strict";
import test from "node:test";
import { readUnifiedAppSourceConfig, unifiedAppSourceView } from "./unified-app-source-config.ts";
import { unifiedMetadata } from "./unified-app-contract.ts";

const body = 'import { buildUnifiedApp } from "@fused/unified-app";\nexport default buildUnifiedApp({ execute() { return { marker: "keep { implementation }" }; } });';
const metadata = { name: "billing", version: "1.0.2", bucket: "default", runtime_buckets: ["default", "deployment"] };

/** Form changes must preserve implementation while updating the default app's config property. */
test("form changes update only the current inline config property", () => {
  const original = body.replace("buildUnifiedApp({", 'buildUnifiedApp({ config: {"bucket":"old"},');
  const updated = unifiedAppSourceView(original, metadata);
  assert.equal(updated.error, "");
  assert.doesNotMatch(updated.source, /export const fusedConfig/);
  assert.deepEqual(readUnifiedAppSourceConfig(updated.source), metadata);
  assert.match(updated.source, /execute\(\) \{ return \{ marker: "keep \{ implementation \}" \}; \}/);
  const fixed = unifiedAppSourceView(updated.source, { name: "billing", version: "1.0.3", bucket: "deployment" });
  assert.doesNotMatch(fixed.source, /runtime_buckets/);
  assert.equal(readUnifiedAppSourceConfig(fixed.source).bucket, "deployment");
});

/** Comments, strings and nested settings cannot claim ownership of the app's direct config property. */
test("metadata discovery ignores examples and nested config", () => {
  const examples = '/* export default buildUnifiedApp({ config: {} }); */\nconst example = `export const fusedConfig = {};`;\n';
  const source = `${examples}export default buildUnifiedApp({ input: z.object({config: z.string()}), /* metadata */ config: {"bucket":"default"}, execute() { const config = {}; return config; } });`;
  assert.deepEqual(readUnifiedAppSourceConfig(source), { bucket: "default" });
  const updated = unifiedAppSourceView(source, metadata);
  assert.equal(updated.error, "");
  assert.ok(updated.source.startsWith(examples));
  assert.match(updated.source, /const config = \{\}; return config/);
});

/** Equivalent source metadata retains exact spacing and native editor selection. */
test("ordinary source edits retain authored config formatting", () => {
  const original = 'export default buildUnifiedApp({ config: { "bucket":"default", "version":"1.0.2", "name":"billing", "runtime_buckets":["default","deployment"] }, execute() {} });';
  assert.deepEqual(unifiedAppSourceView(original, metadata), { source: original, error: "" });
});

/** Implementation-only drafts gain config, while an unfinished generated source remains empty. */
test("source projection handles implementation-only and empty drafts", () => {
  const updated = unifiedAppSourceView(body, metadata);
  assert.equal(updated.error, "");
  assert.deepEqual(readUnifiedAppSourceConfig(updated.source), metadata);
  assert.deepEqual(unifiedAppSourceView("", metadata), { source: "", error: "" });
});

/** Invalid or competing metadata remains available for correction without silently choosing one authority. */
test("malformed and ambiguous metadata blocks synchronization", () => {
  for (const source of ["export const fusedConfig = {", "export default buildUnifiedApp({ config: { bucket: 'default' } });", "export default buildUnifiedApp({ config: getConfig() });", "export default buildUnifiedApp({ config: {}, config: {} });", "export const fusedConfig = {}; export default buildUnifiedApp({ config: {} });", "export default buildUnifiedApp({ config: {}, ...options });"]) {
    const result = unifiedAppSourceView(source, metadata);
    assert.equal(result.source, source);
    assert.match(result.error, /Config could not be synchronized/);
  }
});

/** Retired exports are never accepted or automatically converted by the editor. */
test("only config is accepted by authoring readers", () => {
  for (const source of [`export const fusedConfig = {};\n${body}`, `export const fusedConfig = {};\nexport default buildUnifiedApp({ config: {}, execute() { return fusedConfig.name; } });`]) {
    assert.throws(() => readUnifiedAppSourceConfig(source), /no longer supported/);
    const result = unifiedAppSourceView(source, metadata);
    assert.equal(result.source, source);
    assert.match(result.error, /no longer supported/);
  }
});

/** Every form-owned setting shares one portable metadata projection without recursively embedding the source file. */
test("metadata reflects current provider scope and excludes source transport fields", () => {
  const draft = { name: "billing", description: "Updated description", source: body, webhookAttachment: "billing-events", configSettings: { source: "stale", source_path: "old.ts", bundle_digest: "old", runtime_buckets: ["default", "deployment"] }, services: { Stripe: { service_id: "stripe", service_version_id: "stripe-v1", version: "v1", operations: ["getCustomer"], webhooks: ["invoice.paid"] } }, serviceSettings: { Stripe: { service_id: "stripe", config: { auth: { name: "bearerAuth", type: "bearer" } } } } };
  const config = unifiedMetadata(draft, "billing", "1.0.2", "default");
  assert.deepEqual(config.runtime_buckets, ["default", "deployment"]);
  assert.deepEqual(config.services.Stripe.auth, { name: "bearerAuth", type: "bearer" });
  assert.deepEqual(config.services.Stripe.webhooks, ["invoice.paid"]);
  assert.equal(config.webhook_attachment, "billing-events");
  assert.equal(config.source, undefined);
  assert.equal(config.source_path, undefined);
  assert.equal(config.bundle_digest, undefined);
});
