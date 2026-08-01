import assert from "node:assert/strict";
import test from "node:test";

import { clearApiKey, getApiKey, setApiKey } from "./session.ts";

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
