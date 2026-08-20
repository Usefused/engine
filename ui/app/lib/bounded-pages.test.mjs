import assert from "node:assert/strict";
import test from "node:test";

import { readAllBoundedPages } from "./bounded-pages.ts";

test("collects every server page using the accumulated offset", async () => {
  const offsets = [];
  const items = await readAllBoundedPages(async (limit, offset) => {
    offsets.push([limit, offset]);
    return offset === 0
      ? { items: ["one", "two"], total: 3 }
      : { items: ["three"], total: 3 };
  }, 2, 5);
  assert.deepEqual(items, ["one", "two", "three"]);
  assert.deepEqual(offsets, [[2, 0], [2, 2]]);
});

test("rejects a partial page that cannot advance", async () => {
  await assert.rejects(
    readAllBoundedPages(async () => ({ items: [], total: 1 }), 10, 2),
    /did not advance/
  );
});

test("rejects a catalogue beyond the configured request bound", async () => {
  await assert.rejects(
    readAllBoundedPages(async (_limit, offset) => ({ items: [offset], total: 3 }), 1, 2),
    /exceeds the supported page bound/
  );
});
