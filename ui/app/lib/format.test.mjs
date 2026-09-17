import assert from "node:assert/strict";
import test from "node:test";

import { formatVersion, truncateWords } from "./format.ts";

test("preserves provider-defined version identifiers", () => {
  assert.equal(formatVersion("v2"), "v2");
  assert.equal(formatVersion("2"), "2");
  assert.equal(formatVersion("2026-08"), "2026-08");
});

test("truncateWords leaves text under the budget verbatim", () => {
  const text = "Short service description.";
  assert.equal(truncateWords(text, 100), text);
});

test("truncateWords cuts to the requested word budget on word boundaries", () => {
  const text = "one two three four five";
  assert.equal(truncateWords(text, 3), "one two three\u2026");
});

test("truncateWords collapses surrounding whitespace before counting", () => {
  const text = "  one  two   three  ";
  assert.equal(truncateWords(text, 2), "one two\u2026");
});
