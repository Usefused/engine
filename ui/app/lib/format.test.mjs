import assert from "node:assert/strict";
import test from "node:test";

import { formatDuration, formatVersion, truncateWords } from "./format.ts";

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

// Confirms shared duration formatting keeps sub-second timings in ms and scales larger durations cleanly across unit boundaries.
test("formatDuration converts milliseconds into readable units without impossible boundary rollovers", () => {
  // Invalid and negative inputs return empty string so callers can apply their own missing-value fallback.
  assert.equal(formatDuration(null), "");
  assert.equal(formatDuration(undefined), "");
  assert.equal(formatDuration(Number.NaN), "");
  assert.equal(formatDuration(Number.POSITIVE_INFINITY), "");
  assert.equal(formatDuration(-10), "");

  // Sub-second values stay in milliseconds, preserving fractional precision below 1 ms.
  assert.equal(formatDuration(0), "0 ms");
  assert.equal(formatDuration(0.001), "0 ms");
  assert.equal(formatDuration(0.45), "0.45 ms");
  assert.equal(formatDuration(0.999), "1 ms");
  assert.equal(formatDuration(75), "75 ms");
  assert.equal(formatDuration(999), "999 ms");

  // Rounding across 1000 ms promotes cleanly to seconds and strips trailing zeroes.
  assert.equal(formatDuration(999.6), "1 s");
  assert.equal(formatDuration(1000), "1 s");
  assert.equal(formatDuration(1500), "1.5 s");
  assert.equal(formatDuration(1234), "1.23 s");
  assert.equal(formatDuration(59_990), "59.99 s");

  // Minute, hour, and day tiers promote cleanly at every rounding boundary.
  assert.equal(formatDuration(59_999), "1m");
  assert.equal(formatDuration(60_000), "1m");
  assert.equal(formatDuration(90_400), "1m 30s");
  assert.equal(formatDuration(3_599_400), "59m 59s");
  assert.equal(formatDuration(3_599_600), "1h");
  assert.equal(formatDuration(5_400_000), "1h 30m");
  assert.equal(formatDuration(86_369_000), "23h 59m");
  assert.equal(formatDuration(86_371_000), "1d");
  assert.equal(formatDuration(278_925_548), "3d 5h");
});
