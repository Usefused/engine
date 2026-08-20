import assert from "node:assert/strict";
import test from "node:test";

import { bucketEntryActions } from "./bucket-entry-actions.ts";

test("redacted credentials never advertise copy", () => {
  assert.deepEqual(bucketEntryActions("secret", "********", true), {
    canCopy: false,
    canRemove: true,
  });
});

test("environment values expose copy only when content exists", () => {
  assert.deepEqual(bucketEntryActions("value", "visible-value", false), {
    canCopy: true,
    canRemove: false,
  });
  assert.deepEqual(bucketEntryActions("value", "-", true), {
    canCopy: false,
    canRemove: true,
  });
});
