import assert from "node:assert/strict";
import test from "node:test";

import {
  clearApiKey,
  getApiKey,
  openAuthenticatedTab,
  setApiKey,
} from "./session.ts";

test("keeps the personal key in browser session storage only", () => {
  const values = new Map();
  globalThis.window = {
    sessionStorage: {
      getItem: (key) => values.get(key) ?? null,
      setItem: (key, value) => values.set(key, value),
      removeItem: (key) => values.delete(key),
    },
  };
  setApiKey("fsk_test_secret");
  assert.equal(getApiKey(), "fsk_test_secret");
  clearApiKey();
  assert.equal(getApiKey(), null);
  delete globalThis.window;
});

test("falls back to page memory when browser storage is unavailable", () => {
  globalThis.window = {
    sessionStorage: {
      getItem: () => { throw new Error("storage disabled"); },
      setItem: () => { throw new Error("storage disabled"); },
      removeItem: () => { throw new Error("storage disabled"); },
    },
  };
  setApiKey("fsk_memory_only");
  assert.equal(getApiKey(), "fsk_memory_only");
  clearApiKey();
  assert.equal(getApiKey(), null);
  delete globalThis.window;
});

test("hands the active session to a same-origin tab and detaches its opener", () => {
  const parentValues = new Map();
  const childValues = new Map();
  const navigations = [];
  const child = {
    opener: {},
    sessionStorage: {
      setItem: (key, value) => childValues.set(key, value),
    },
    location: {
      replace: (path) => navigations.push(path),
    },
  };
  globalThis.window = {
    sessionStorage: {
      getItem: (key) => parentValues.get(key) ?? null,
      setItem: (key, value) => parentValues.set(key, value),
      removeItem: (key) => parentValues.delete(key),
    },
    open: (url, target) => {
      assert.equal(url, "about:blank");
      assert.equal(target, "_blank");
      return child;
    },
  };

  setApiKey("fsk_new_tab_secret");
  assert.equal(openAuthenticatedTab("/integrations/jira"), true);
  assert.equal(childValues.get("fused_api_key"), "fsk_new_tab_secret");
  assert.equal(child.opener, null);
  assert.deepEqual(navigations, ["/integrations/jira"]);

  clearApiKey();
  delete globalThis.window;
});

test("does not hand credentials to an external destination", () => {
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
