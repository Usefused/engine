import assert from "node:assert/strict";
import test from "node:test";

import {
  getCSRFToken,
  openAuthenticatedTab,
  purgeLegacyBrowserCredential,
} from "./session.ts";

test("removes the legacy browser-stored API key", () => {
  const removed = [];
  globalThis.window = {
    sessionStorage: { removeItem: (key) => removed.push(key) },
  };
  purgeLegacyBrowserCredential();
  assert.deepEqual(removed, ["fused_api_key"]);
  delete globalThis.window;
});

test("reads only the non-HttpOnly CSRF cookie", () => {
  globalThis.document = {
    cookie: "other=value; __Host-fused_csrf=csrf%2Dtoken",
  };
  assert.equal(getCSRFToken(), "csrf-token");
  delete globalThis.document;
});

test("ignores malformed CSRF cookies and uses the deterministic fallback", () => {
  globalThis.document = {
    cookie: "fused_csrf_dev=dev-token; __Host-fused_csrf=%E0%A4%A",
  };
  assert.equal(getCSRFToken(), "dev-token");
  delete globalThis.document;
});

test("opens a same-origin tab without copying credentials", () => {
  let opened;
  let destination;
  const child = {
    opener: {},
    location: { replace: (path) => { destination = path; } },
    close: () => undefined,
  };
  globalThis.window = {
    open: (...args) => {
      opened = args;
      return child;
    },
  };
  assert.equal(openAuthenticatedTab("/integrations/jira"), true);
  assert.deepEqual(opened, ["about:blank", "_blank"]);
  assert.equal(child.opener, null);
  assert.equal(destination, "/integrations/jira");
  delete globalThis.window;
});

test("does not open an external destination", () => {
  let opened = false;
  globalThis.window = {
    open: () => {
      opened = true;
      return null;
    },
  };
  assert.equal(openAuthenticatedTab("https://example.com"), false);
  assert.equal(openAuthenticatedTab("//example.com/integrations"), false);
  assert.equal(opened, false);
  delete globalThis.window;
});
