import assert from "node:assert/strict";
import test from "node:test";
import { bucketServiceIdentity, bucketSecretIdentity } from "./bucket-service-identity.ts";

// Registry metadata must name records even after their workspace service was removed.
test("Registry names distinguish identical OAuth credential families", () => {
  const base = { service_id: "gmail-id", service_name: "Gmail", service_slug: "@google/gmail", key_name: "oauth2", credential_type: "oauth" };
  assert.deepEqual(bucketSecretIdentity(base), { name: "Gmail · OAuth credentials", detail: "@google/gmail · Auth scheme: oauth2" });
  assert.equal(bucketSecretIdentity({ ...base, service_name: "Google Drive" }).name, "Google Drive · OAuth credentials");
  assert.match(bucketSecretIdentity({ ...base, key_name: "admin_oauth" }).detail, /Auth scheme: admin_oauth$/);
});

// Registry outages can still use locally loaded identity and auth scheme labels.
test("workspace metadata supplies readable identity and exact scheme labels", () => {
  const services = [{ service_id: "gmail-id", service_name: "Gmail", service_slug: "gmail", auth_options: [{ key_prefix: "oauth2", label: "Google login" }] }];
  assert.deepEqual(bucketServiceIdentity({ service_id: "gmail-id", service_name: "" }, services), { name: "Gmail", detail: "gmail" });
  assert.equal(bucketSecretIdentity({ service_id: "gmail-id", credential_type: "oauth", key_name: "oauth2" }, services).detail, "gmail · Auth scheme: Google login (oauth2)");
});

// Unknown services remain distinguishable without presenting a UUID as a service name.
test("missing metadata is explicit and generic bucket secrets stay independent", () => {
  assert.deepEqual(bucketServiceIdentity({ service_id: "orphan-id" }), { name: "Service unavailable", detail: "Service ID: orphan-id" });
  assert.deepEqual(bucketSecretIdentity({ credential_type: "bucket_secret", key_name: "secret:signing_key" }), { name: "Secret", detail: "secret:signing_key" });
  assert.equal(bucketServiceIdentity({ service_id: "id", service_slug: "gmail" }).name, "gmail");
});
