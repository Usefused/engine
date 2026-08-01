import assert from "node:assert/strict";
import test from "node:test";

import { safeInternalPath } from "./safe-navigation.ts";

test("keeps login return navigation on the Engine origin", () => {
  assert.equal(safeInternalPath("/integrations/sdk-builder?draft=1#owner"), "/integrations/sdk-builder?draft=1#owner");
  for (const malicious of ["//evil.example", "\\\\evil.example", "/\\evil.example", "https://evil.example", "javascript:alert(1)", " data:text/html,x"]) {
    assert.equal(safeInternalPath(malicious), "/integrations");
  }
});
