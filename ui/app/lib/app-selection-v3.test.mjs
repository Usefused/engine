import assert from "node:assert/strict";
import test from "node:test";

import {
  APP_SELECTION_DEFINITION_SCHEMA_VERSION,
  appSelectionDisplayRows,
  requireAppSelectionV3,
  requireAppSelectionsV3,
} from "./app-selection-v3.ts";

// selection returns one complete v3 fixture so each test varies one invariant.
function selection(overrides = {}) {
  return {
    service_id: "service-1",
    service_version_id: "version-1",
    definition_schema_version: APP_SELECTION_DEFINITION_SCHEMA_VERSION,
    endpoint_ids: ["endpoint-1"],
    webhook_ids: [],
    select_all: false,
    webhook_select_all: false,
    ...overrides,
  };
}

test("accepts selections that pin the v3 schema and service version", () => {
  assert.deepEqual(requireAppSelectionsV3([selection()]), [selection()]);
});

test("rejects legacy selection schemas before the detail view consumes them", () => {
  assert.throws(
    () => requireAppSelectionV3(selection({ definition_schema_version: 2 })),
    /unsupported selection schema version/
  );
});

test("rejects a v3 selection without an immutable service version", () => {
  assert.throws(
    () => requireAppSelectionV3(selection({ service_version_id: null })),
    /missing its service version/
  );
});

test("renders immutable operation names without positionally pairing endpoint IDs", () => {
  assert.deepEqual(
    appSelectionDisplayRows(
      ["endpoint-1", "endpoint-2"],
      ["listProjects", "createIssue"]
    ),
    [
      { id: "semantic:0:listProjects", name: "listProjects" },
      { id: "semantic:1:createIssue", name: "createIssue" },
    ]
  );
});

test("keeps historical ID-only selections visible without a Registry lookup", () => {
  assert.deepEqual(appSelectionDisplayRows(["endpoint-1"], undefined), [
    { id: "endpoint-1", name: "endpoint-1" },
  ]);
});
