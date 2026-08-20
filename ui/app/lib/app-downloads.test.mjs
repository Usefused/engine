import assert from "node:assert/strict";
import test from "node:test";

import { formatAppDownloadCount } from "./app-downloads.ts";

// This test keeps zero distinct from unavailable and preserves large counts.
test("formats durable app download counts without numeric narrowing", () => {
  assert.equal(formatAppDownloadCount("0"), "0");
  assert.equal(formatAppDownloadCount("1234"), BigInt(1234).toLocaleString());
  assert.equal(formatAppDownloadCount("9007199254740993"), BigInt("9007199254740993").toLocaleString());
  assert.equal(formatAppDownloadCount(null), "—");
  assert.equal(formatAppDownloadCount("-1"), "—");
});
