import assert from "node:assert/strict";
import test from "node:test";

import {
  CREATE_CREDENTIAL_PATH,
  consumeCredentialCreationRequest,
  requestsCredentialCreation,
} from "./credential-navigation.ts";

test("opens the credential page with a one-shot creation request", () => {
  assert.equal(CREATE_CREDENTIAL_PATH, "/integrations/buckets?create=credential");
  const params = new URLSearchParams("create=credential&bucket=existing");
  assert.equal(requestsCredentialCreation(params), true);

  const consumed = consumeCredentialCreationRequest(params);
  assert.equal(consumed.get("create"), null);
  assert.equal(consumed.get("bucket"), "existing");
  assert.equal(params.get("create"), "credential");
});

test("ignores unrelated credential-page navigation", () => {
  assert.equal(requestsCredentialCreation(new URLSearchParams("tab=secrets")), false);
  assert.equal(requestsCredentialCreation(new URLSearchParams("create=value")), false);
});
