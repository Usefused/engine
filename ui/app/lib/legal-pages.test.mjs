import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { URL } from "node:url";
import test from "node:test";

const privacyRouteSource = readFileSync(new URL("../routes/privacy-policy.tsx", import.meta.url), "utf8");
const termsRouteSource = readFileSync(new URL("../routes/terms-of-service.tsx", import.meta.url), "utf8");
const sidebarSource = readFileSync(new URL("../components/layout/IntegrationsSidebar.tsx", import.meta.url), "utf8");

// Legacy Engine URLs must preserve old bookmarks while keeping the homepage documents canonical.
test("legacy legal routes redirect to the canonical homepage policies", () => {
  assert.match(privacyRouteSource, /https:\/\/usefused\.com\/legal\/privacy-policy/);
  assert.match(privacyRouteSource, /window\.location\.replace\(PRIVACY_POLICY_URL\)/);
  assert.match(termsRouteSource, /https:\/\/usefused\.com\/legal\/terms-of-service/);
  assert.match(termsRouteSource, /window\.location\.replace\(TERMS_OF_SERVICE_URL\)/);
});

// Authenticated navigation must link directly to the same canonical public documents.
test("Engine navigation links to the canonical homepage policies", () => {
  assert.match(sidebarSource, /href="https:\/\/usefused\.com\/legal\/privacy-policy"/);
  assert.match(sidebarSource, /href="https:\/\/usefused\.com\/legal\/terms-of-service"/);
});
