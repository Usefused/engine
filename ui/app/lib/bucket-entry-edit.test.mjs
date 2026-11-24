import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";

const modelSource = readFileSync(new URL("../components/buckets/BucketEntryComposerModel.ts", import.meta.url), "utf8");
const compiled = ts.transpileModule(modelSource, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022 },
}).outputText;
const model = {};
new Function("exports", compiled)(model);

const services = [
  {
    service_id: "svc-openai",
    service_name: "OpenAI",
    auth_options: [
      { id: "opt-api-key", auth_type: "api_key", credential_type: "api_key", key_name: "ApiKeyAuth", key_prefix: "ApiKeyAuth", label: "API key", required_fields: ["value"] },
      { id: "opt-oauth", auth_type: "oauth2", credential_type: "oauth", key_name: "oauth2", key_prefix: "oauth2", label: "OAuth", required_fields: ["client_id", "client_secret"] },
    ],
  },
];

// Generic bucket secrets strip the internal "secret:" prefix so updating overwrites the existing row in place.
test("bucketSecretEditTarget resolves generic secrets and strips storage prefix", () => {
  const secret = { id: "s1", bucket_id: "b1", service_id: "", key_name: "secret:stripe_signing", credential_type: "bucket_secret", exportable: true, expires_at: "2026-12-01T10:30:00Z" };
  const target = model.bucketSecretEditTarget(secret, services);
  assert.equal(target?.kind, "bucket_secret");
  const draft = model.editTargetInitialDraft(target);
  assert.equal(draft.bucketSecret.keyName, "stripe_signing");
  assert.equal(draft.bucketSecret.value, "");
  assert.equal(draft.bucketSecret.exportable, true);
  assert.match(draft.bucketSecret.expiresAt, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
});

// Service auth secrets match their configured auth option and keep secret inputs write-only.
test("bucketSecretEditTarget matches service auth schemes and rejects orphaned services", () => {
  const secret = { id: "s2", bucket_id: "b1", service_id: "svc-openai", key_name: "ApiKeyAuth", credential_type: "api_key", exportable: false };
  const target = model.bucketSecretEditTarget(secret, services);
  assert.equal(target?.kind, "secret");
  assert.equal(target?.authOptionId, "opt-api-key");
  const draft = model.editTargetInitialDraft(target);
  assert.equal(draft.secret.serviceId, "svc-openai");
  assert.equal(draft.secret.credentialType, "opt-api-key");
  assert.equal(draft.secret.value, "");
  assert.equal(draft.secret.exportable, false);

  const orphan = { id: "s3", bucket_id: "b1", service_id: "svc-deleted", key_name: "ApiKeyAuth", credential_type: "api_key" };
  assert.equal(model.bucketSecretEditTarget(orphan, services), null);
});

// Environment values pre-fill their service, key name, location, and current value for in-place editing.
test("editTargetInitialDraft pre-fills env value entries", () => {
  const draft = model.editTargetInitialDraft({
    kind: "value",
    value: { bucket_id: "b1", service_id: "svc-openai", key_name: "ORG_ID", location: "header", value: "org_123" },
  });
  assert.deepEqual(draft.value, { serviceId: "svc-openai", keyName: "ORG_ID", location: "header", value: "org_123" });
});
