import assert from "node:assert/strict";
import test from "node:test";

import { credentialedRequestInit, credentialedResponseLoginPath } from "./browser-request.ts";

test("credentialed API requests attach cookies and CSRF only to unsafe methods", async () => {
  const get = credentialedRequestInit({}, "csrf-token");
  const post = credentialedRequestInit({ method: "POST", body: "{}" }, "csrf-token");
  assert.equal(get.credentials, "include");
  assert.equal(get.headers.get("X-Fused-CSRF"), null);
  assert.equal(post.credentials, "include");
  assert.equal(post.headers.get("X-Fused-CSRF"), "csrf-token");
});

test("shared 401 handling preserves the complete destination and avoids login loops", () => {
  assert.equal(
    credentialedResponseLoginPath(401, "/integrations", "?handoff=cli&session=opaque-run&tab=pending"),
    "/login?next=%2Fintegrations%3Fhandoff%3Dcli%26session%3Dopaque-run%26tab%3Dpending",
  );
  assert.equal(credentialedResponseLoginPath(200, "/integrations", "?session=opaque-run"), null);
  assert.equal(credentialedResponseLoginPath(401, "/login", "?next=%2Fintegrations"), null);
  assert.equal(credentialedResponseLoginPath(401, "/cli-login", ""), null);
});
