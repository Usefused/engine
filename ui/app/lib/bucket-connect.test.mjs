import assert from "node:assert/strict";
import test from "node:test";
import { bucketAuthorizeURL, bucketConnectOptions, bucketConnectReturnURL, bucketConnectVariables, selectedConnectOption, canConnectBucketService } from "./bucket-connect.ts";

const oauth = { id: "oauth:primary", label: "OAuth", auth_type: "oauth", credential_type: "oauth", key_prefix: "primary", supports_connected_users: true };
const oidc = { ...oauth, id: "oidc:other", credential_type: "oidc", key_prefix: "other" };
const input = { bucketId: "chosen-bucket", serviceId: "chosen-service", endUserRef: " account-123 ", auth: oauth, authRef: "", scopes: "" };

// CLI parity requires exact identity and no fabricated app attribution or credential defaults.
test("preserves CLI selectors and omits unspecified overrides", () => {
  const variables = bucketConnectVariables(input, "https://engine.example");
  assert.equal(variables.bucketId, "chosen-bucket");
  assert.equal(variables.serviceId, "chosen-service");
  assert.equal(variables.endUserRef, "account-123");
  assert.equal(variables.authType, "oauth");
  assert.equal(variables.authName, "primary");
  const sent = JSON.parse(JSON.stringify(variables));
  assert.equal("scopes" in sent, false);
  assert.equal("authRef" in sent, false);
  assert.equal("createdByAppId" in sent, false);
  assert.equal(variables.returnUrl, "https://engine.example/integrations/buckets?bucket=chosen-bucket&tab=connected-users");
});

// Explicit managed references reach the existing Engine resolver unchanged.
test("forwards references and scopes without duplicating provider policy", () => {
  const authRef = "${fused.bucket.auth.calendar.primary}";
  const variables = bucketConnectVariables({ ...input, auth: oidc, authRef, scopes: "openid\nprofile  email" }, "https://engine.example");
  assert.equal(variables.authRef, authRef);
  assert.equal(variables.authType, "oidc");
  assert.equal(variables.authName, "other");
  assert.deepEqual(variables.scopes, ["openid", "profile", "email"]);
});

// Missing metadata and ambiguous schemes must never select unrelated credentials.
test("filters connected-user schemes and requires explicit ambiguous selection", () => {
  assert.deepEqual(bucketConnectOptions(), []);
  assert.deepEqual(bucketConnectOptions({ auth_options: [oauth, { ...oauth, supports_connected_users: false }] }), [oauth]);
  assert.equal(selectedConnectOption([oauth], ""), oauth);
  assert.equal(selectedConnectOption([oauth], "removed-scheme"), undefined);
  assert.equal(selectedConnectOption([oauth, oidc], ""), undefined);
  assert.equal(selectedConnectOption([oauth, oidc], oidc.id), oidc);
});

// Local validation prevents requests without the account or exact target; Engine owns provider admission.
test("rejects missing user or exact selection", () => {
  assert.throws(() => bucketConnectVariables({ ...input, endUserRef: "  " }, "https://engine.example"), /user reference/);
  assert.throws(() => bucketConnectVariables({ ...input, serviceId: "" }, "https://engine.example"), /Choose a service/);
  assert.throws(() => bucketConnectVariables({ ...input, auth: { ...oauth, key_prefix: "" } }, "https://engine.example"), /Choose a service/);
});

// Only web handoffs are navigable, and return links never inherit unrelated or attacker-supplied query fields.
test("restricts handoff protocols and encodes bucket identity", () => {
  assert.throws(() => bucketAuthorizeURL("javascript:alert(1)"), /invalid connection URL/);
  assert.throws(() => bucketAuthorizeURL("data:text/html,test"), /invalid connection URL/);
  assert.equal(bucketAuthorizeURL("https://provider.example/authorize?state=example"), "https://provider.example/authorize?state=example");
  assert.equal(bucketAuthorizeURL("http://127.0.0.1:57562/workspace/connect/input/test"), "http://127.0.0.1:57562/workspace/connect/input/test");
  const url = new URL(bucketConnectReturnURL("https://engine.example", "bucket&next=external"));
  assert.equal(url.searchParams.get("bucket"), "bucket&next=external");
  assert.equal(url.searchParams.has("next"), false);
});

// Each permission is independently required, including for an actor who can administer other buckets.
test("connection control requires use and manage of this bucket plus consumption of this service", () => {
  const grants = [
    { permission: "connection.manage", resource_type: "BUCKET", resource_id: "bucket" },
    { permission: "bucket.use", resource_type: "BUCKET", resource_id: "bucket" },
    { permission: "service.consume", resource_type: "SERVICE", resource_id: "service" },
  ];
  const access = { workspace_id: "workspace", grants };
  assert.equal(canConnectBucketService(access, "bucket", "service"), true);
  assert.equal(canConnectBucketService(access, "other", "service"), false);
  assert.equal(canConnectBucketService(access, "bucket", "other"), false);
  assert.equal(canConnectBucketService(access, "bucket"), false);
  assert.equal(canConnectBucketService(null, "bucket", "service"), false);
  for (let omitted = 0; omitted < grants.length; omitted++) {
    assert.equal(canConnectBucketService({ ...access, grants: grants.filter((_, index) => index !== omitted) }, "bucket", "service"), false);
  }
});
