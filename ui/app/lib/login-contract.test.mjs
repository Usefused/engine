import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { URL } from "node:url";
import test from "node:test";

const loginSource = readFileSync(new URL("../routes/login.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("./api.ts", import.meta.url), "utf8");
const browserRequestSource = readFileSync(new URL("./browser-request.ts", import.meta.url), "utf8");
const cliLoginSource = readFileSync(new URL("../routes/cli-login.tsx", import.meta.url), "utf8");
const integrationsSource = readFileSync(new URL("../routes/integrations.tsx", import.meta.url), "utf8");

test("uses managed sign-in with API Key administrator access", () => {
  assert.match(loginSource, /await api\.auth\.startManaged\(\)/);
  assert.match(loginSource, /await api\.auth\.pollManaged/);
  assert.match(loginSource, /await api\.auth\.exchangeAPIKey\(value\)/);
  assert.match(loginSource, />API Key</);
  assert.match(loginSource, /Use API Key/);
  assert.doesNotMatch(loginSource, /Administrator recovery|>License Key</);
  assert.match(loginSource, /popup\.opener = null/);
  assert.match(loginSource, /window\.open\("about:blank", "_blank"\)/);
  assert.doesNotMatch(loginSource, /"fused-managed-login"/);
  assert.doesNotMatch(loginSource, /popup,width=/);
  assert.doesNotMatch(loginSource, /setApiKey|sessionStorage/);
});

test("keeps managed sign-in failures generic and forces identity choice only on retry", () => {
  assert.match(loginSource, /MANAGED_SIGN_IN_ERROR = "Sign-in could not be completed\."/);
  assert.match(loginSource, /searchParams\.set\("reauthenticate", "true"\)/);
  assert.match(loginSource, /managedRetry \? "Try again" : "Continue with email or SSO"/);
  assert.doesNotMatch(loginSource, /invited to this Engine|Logto/i);
});

test("approves CLI enrollment without exposing a generated credential", () => {
  assert.match(loginSource, /loginDestination\(next\)/);
  assert.match(cliLoginSource, /window\.location\.hash/);
  assert.match(cliLoginSource, /window\.history\.replaceState/);
  assert.match(cliLoginSource, /api\.auth\.approveCLI/);
  assert.match(apiSource, /\/auth\/cli\/approve/);
  assert.doesNotMatch(cliLoginSource, /api[_-]?key|credential_hash|poll_token/i);
});

test("keeps embedded API requests on the Engine origin", () => {
  assert.match(apiSource, /__FUSED_ENV\?\.BACKEND_URL/);
  assert.match(apiSource, /return "";/);
	assert.doesNotMatch(apiSource, /return "http:\/\/localhost:8081"/);
	assert.match(apiSource, /requires a same-origin BACKEND_URL/);
});

test("uses cookie credentials and credential-bound CSRF without browser API keys", () => {
  assert.match(browserRequestSource, /credentials: "include"/);
  assert.match(browserRequestSource, /headers\.set\("X-Fused-CSRF", csrfToken\)/);
  assert.doesNotMatch(apiSource, /getApiKey\(\)/);
});

test("uses top-level provider logout and handles an already-revoked local session", () => {
  assert.match(apiSource, /logout_url\?: string/);
  assert.match(integrationsSource, /window\.location\.assign\(logoutURL\)/);
  assert.match(integrationsSource, /if \(!session\.authenticated\)/);
  assert.match(integrationsSource, /window\.location\.replace\("\/login"\)/);
});
