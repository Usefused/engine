import assert from "node:assert/strict";
import test from "node:test";
import { versionStateLabel } from "./service-version-visibility.ts";

// Legacy lifecycle status must not turn a deliberately private version into a public label.
test("version labels follow visibility and preserve lifecycle warnings", () => {
  assert.equal(versionStateLabel({ is_public: false, status: "public" }), "Private");
  assert.equal(versionStateLabel({ is_public: true, status: "public" }), "Public");
  assert.equal(versionStateLabel({ is_public: false }), "Private");
  assert.equal(versionStateLabel({ is_public: true, status: "deprecated" }), "Deprecated");
  assert.equal(versionStateLabel({ is_public: false, status: "draft" }), "Draft");
});
