import assert from "node:assert/strict";
import test from "node:test";

import { formatVersion } from "./format.ts";

test("preserves provider-defined version identifiers", () => {
  assert.equal(formatVersion("v2"), "v2");
  assert.equal(formatVersion("2"), "2");
  assert.equal(formatVersion("2026-08"), "2026-08");
});
