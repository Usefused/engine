import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

// Reads the shared operation row so every endpoint catalogue keeps the same disclosure behavior.
function endpointRowSource() {
  return readFileSync(new URL("../components/EndpointRow.tsx", import.meta.url), "utf8");
}

// Long descriptions must stay compact without making short descriptions unnecessarily interactive.
test("endpoint descriptions expand only after a bounded collapsed preview", () => {
  const source = endpointRowSource();

  assert.match(source, /ENDPOINT_DESCRIPTION_COLLAPSE_THRESHOLD = 240/);
  assert.match(source, /collapsible && !expanded \? "line-clamp-3"/);
  assert.match(source, /aria-expanded=\{expanded\}/);
  assert.match(source, /expanded \? "Show less" : "Show more"/);
  assert.match(source, /event\.stopPropagation\(\)/);
  assert.match(source, /ep\.description \? <EndpointDescription/);
});
