import assert from "node:assert/strict";
import test from "node:test";
import { importedMcpEnabled } from "./features.ts";

// Older shells, missing values, and strings must never opt users into the preview.
test("imported MCP requires the Engine boolean", () => {
 const original = globalThis.window;
 try {
  delete globalThis.window;
  assert.equal(importedMcpEnabled(), false);
  for (const value of [undefined, false, "true", true]) {
   globalThis.window = { __FUSED_ENV: { IMPORTED_MCP_ENABLED: value } };
   assert.equal(importedMcpEnabled(), value === true);
  }
 } finally {
  globalThis.window = original;
 }
});
