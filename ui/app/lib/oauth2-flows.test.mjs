import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  firstAvailableOAuth2Flow,
  oauth2FlowEntries,
  oauth2ScopeNames,
  removeOAuth2Flow,
  renameOAuth2Flow,
  replaceOAuth2Scopes,
  updateOAuth2Flow,
} from "./oauth2-flows.ts";

const authorizationCode = {
  authorization_url: "https://auth.example/authorize",
  token_url: "https://auth.example/token",
  scopes: { write: "Write records", read: "Read records" },
};

test("preserves canonical OAuth2 flow maps without singular projections", () => {
  const auth = updateOAuth2Flow({ type: "oauth2" }, "authorizationCode", authorizationCode);
  assert.deepEqual(oauth2FlowEntries(auth), [["authorizationCode", authorizationCode]]);
  assert.equal(firstAvailableOAuth2Flow(auth), "clientCredentials");
  assert.equal("flow" in auth, false);
  assert.equal("token_url" in auth, false);
  assert.equal("scopes" in auth, false);
});

test("renames and removes only the selected flow", () => {
  const auth = { type: "oauth2", oauth2_flows: { authorizationCode, implicit: { scopes: {} } } };
  const renamed = renameOAuth2Flow(auth, "implicit", "password");
  assert.deepEqual(Object.keys(renamed.oauth2_flows).sort(), ["authorizationCode", "password"]);
  assert.deepEqual(removeOAuth2Flow(renamed, "password").oauth2_flows, { authorizationCode });
});

test("normalizes scope names while preserving existing descriptions", () => {
  const flow = replaceOAuth2Scopes(authorizationCode, [" read ", "new", "read", ""]);
  assert.deepEqual(oauth2ScopeNames(flow), ["new", "read"]);
  assert.equal(flow.scopes.read, "Read records");
  assert.equal(flow.scopes.new, "");
});

test("UI queries and renderers consume only the canonical OAuth2 flow map", () => {
  const route = readFileSync(new URL("../routes/integrations.$id.tsx", import.meta.url), "utf8");
  const editor = readFileSync(new URL("../components/integration-details/AuthConfigSection.tsx", import.meta.url), "utf8");
  const directSingularAccess = /auth\.(?:flow|token_url|authorization_url|scopes)\b/;
  const singularGraphQLSelection = /auth_configs\s*\{[^}]*\b(?:flow|token_url|authorization_url|scopes)\b[^}]*\}/s;

  assert.doesNotMatch(route, directSingularAccess);
  assert.doesNotMatch(editor, directSingularAccess);
  assert.doesNotMatch(route, singularGraphQLSelection);
  assert.match(route, /auth_configs\s*\{[^}]*\boauth2_flows\b[^}]*\}/s);
});
