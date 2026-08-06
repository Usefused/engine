import assert from "node:assert/strict";
import test from "node:test";

import { credentialedRequestInit } from "./browser-request.ts";

test("credentialed API requests attach cookies and CSRF only to unsafe methods", async () => {
  const get = credentialedRequestInit({}, "csrf-token");
  const post = credentialedRequestInit({ method: "POST", body: "{}" }, "csrf-token");
  assert.equal(get.credentials, "include");
  assert.equal(get.headers.get("X-Fused-CSRF"), null);
  assert.equal(post.credentials, "include");
  assert.equal(post.headers.get("X-Fused-CSRF"), "csrf-token");
});
