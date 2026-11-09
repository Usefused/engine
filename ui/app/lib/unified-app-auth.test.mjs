import assert from "node:assert/strict";
import test from "node:test";
import { applyAppAuthEdits } from "./app-service-auth.ts";
import { unifiedAuthOptions } from "./unified-app-auth.ts";

// Current service-auth controls must retain unrelated settings and reject an auth edit for a different provider.
test("service auth edits preserve provider routing", () => {
  for (const kind of ["sdk", "mcp", "unified_app"]) {
    const config = { kind, services: { stripe: { version: "v1", operations: ["postCustomers"], auth: { type: "basic", name: "basicAuth" }, bucket: "billing" } } };
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

// Named schemes of the same type stay distinct choices in the current service-auth selector.
test("auth options preserve named identities", () => {
  const options = unifiedAuthOptions([{ type: "http", scheme: "bearer", name: "bearerAuth" }, { type: "bearer", name: "adminBearer" }, { type: "http", scheme: "basic", name: "basicAuth" }]);
  assert.deepEqual(options.map(({ type, name }) => ({ type, name })), [{ type: "bearer", name: "bearerAuth" }, { type: "bearer", name: "adminBearer" }, { type: "basic", name: "basicAuth" }]);
  assert.notEqual(options[0].key, options[1].key);
});
