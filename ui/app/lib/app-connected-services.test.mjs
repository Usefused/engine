import assert from "node:assert/strict";
import test from "node:test";
import { appConnectedServiceSelections } from "./app-connected-services.ts";
import { APP_SELECTION_DEFINITION_SCHEMA_VERSION } from "./app-selection-v3.ts";

function selection(overrides = {}) {
  return {
    service_id: "jira-service",
    service_version_id: "jira-version",
    definition_schema_version: APP_SELECTION_DEFINITION_SCHEMA_VERSION,
    endpoint_ids: ["endpoint-uuid"],
    operation_names: ["createIssue"],
    webhook_ids: [],
    webhook_names: [],
    select_all: false,
    webhook_select_all: false,
    ...overrides,
  };
}

test("joins one batched service summary without replacing immutable operation names", () => {
  const result = appConnectedServiceSelections([selection()], [{
    service_id: "jira-service",
    service_slug: "@atlassian/jira",
    service_name: "Jira",
    version: "2026-08",
    select_all: false,
    endpoint_count: 42,
    webhook_count: 3,
  }]);

  assert.equal(result.length, 1);
  assert.equal(result[0].service_name, "Jira");
  assert.equal(result[0].service_slug, "@atlassian/jira");
  assert.equal(result[0].service_version_name, "2026-08");
  assert.deepEqual(result[0].operation_names, ["createIssue"]);
  assert.deepEqual(result[0].endpoint_ids, ["endpoint-uuid"]);
});

test("keeps an authorized immutable selection visible when its label snapshot is unavailable", () => {
  const result = appConnectedServiceSelections([selection()], []);
  assert.equal(result[0].service_name, undefined);
  assert.deepEqual(result[0].operation_names, ["createIssue"]);
});

test("rejects legacy selection schemas before either app detail adapter renders them", () => {
  assert.throws(
    () => appConnectedServiceSelections([selection({ definition_schema_version: 2 })], []),
    /unsupported selection schema version/,
  );
});
