import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { mutableWorkspaceNotificationID } from "./workspace-notification.ts";

const apiPath = fileURLToPath(import.meta.resolve("./api.ts"));
const integrationsIndexPath = fileURLToPath(import.meta.resolve("../routes/integrations._index.tsx"));
const integrationDetailPath = fileURLToPath(import.meta.resolve("../routes/integrations.$id.tsx"));

// sourceSection isolates one API contract so unrelated fields cannot satisfy assertions.
function sourceSection(source, start, end) {
  const startIndex = source.indexOf(start);
  const endIndex = source.indexOf(end, startIndex + start.length);
  assert.notEqual(startIndex, -1, `missing section start: ${start}`);
  assert.notEqual(endIndex, -1, `missing section end: ${end}`);
  return source.slice(startIndex, endIndex);
}

test("workspace membership uses exact enabled versions without a false workspace field", async () => {
  const source = await readFile(apiPath, "utf8");
  const section = sourceSection(source, "getServices: () =>", "listWebhookEvents:");

  assert.match(section, /enabled_versions\s*\{[^}]*service_version_id/s);
  assert.doesNotMatch(section, /\bworkspace_id\b/);
});

test("execution history requests every v3 receipt diagnostic", async () => {
  const source = await readFile(apiPath, "utf8");
  const section = sourceSection(source, "const engineExecutionEventSelection", "export interface EngineExecutionAnalyticsSummary");
  const fields = [
    "auth_scheme_names",
    "auth_scheme_types",
    "auth_scheme_count",
    "auth_selection_outcome",
    "pagination_type",
    "pagination_page_count",
    "pagination_item_count",
    "pagination_byte_count",
    "pagination_stop_reason",
    "rate_limit_decision",
    "rate_limit_policy_count",
    "rate_limit_scope_kinds",
    "rate_limit_units",
    "rate_limit_unit_totals",
    "rate_limit_retry_outcome",
    "rate_limit_header_outcome",
  ];

  for (const field of fields) assert.match(section, new RegExp(`\\b${field}\\b`));
});

test("public insights forwards exact version, object, and transport scopes", async () => {
  const source = await readFile(apiPath, "utf8");
  const section = sourceSection(source, "getPublicServiceInsights:", "listServiceConsumers:");

  assert.match(section, /service_version_id: \$serviceVersionId/);
  assert.match(section, /registry_object_kind: \$registryObjectKind/);
  assert.match(section, /registry_object_id: \$registryObjectId/);
  assert.match(section, /transport: \$transport/);
  assert.match(section, /serviceVersionId: params\.serviceVersionId/);
});

test("connection profiles request v3 identity, visibility, and literal bindings", async () => {
  const source = await readFile(apiPath, "utf8");
  const selection = sourceSection(source, "const workspaceConnectionProfileSelection", "export interface SecretMeta");
  const methods = sourceSection(source, "getWorkspaceConnectionProfile:", "listNotifications:");

  for (const field of ["registry_profile_id", "is_public", "literal_value"]) {
    assert.match(selection, new RegExp(`\\b${field}\\b`));
  }
  // Read, set, and reset must return the same effective-profile shape.
  assert.equal(methods.match(/\$\{workspaceConnectionProfileSelection\}/g)?.length, 3);
  assert.doesNotMatch(selection, /\blocally_overridden\b/);
});

test("specification import apply submits the reviewed artifact receipt", async () => {
  const [apiSource, indexSource, detailSource] = await Promise.all([
    readFile(apiPath, "utf8"),
    readFile(integrationsIndexPath, "utf8"),
    readFile(integrationDetailPath, "utf8"),
  ]);
  const planType = sourceSection(apiSource, "export interface SpecificationImportPlan", "export interface SpecificationImportApplyResult");
  const importsAPI = sourceSection(apiSource, "integrations: {", "start: (");

  assert.match(planType, /\breview_hash:\s*string/);
  assert.match(importsAPI, /applyImport:\s*\(planId:\s*string,\s*reviewHash:\s*string\)/);
  assert.match(importsAPI, /JSON\.stringify\(\{\s*plan_id:\s*planId,\s*review_hash:\s*reviewHash\s*\}\)/);
  assert.doesNotMatch(importsAPI, /applyImport[\s\S]*source_hash:\s*sourceHash/);
  assert.match(indexSource, /applyImport\(importPlan\.plan_id,\s*importPlan\.review_hash\)/);
  assert.match(detailSource, /applyImport\(plan\.plan_id,\s*plan\.review_hash\)/);
});

test("service catalog requests only public Registry services", async () => {
  const source = await readFile(integrationsIndexPath, "utf8");
  const catalogQuery = sourceSection(source, "const CATALOG_SEARCH_QUERY", "// searchCatalogServices");

  assert.match(catalogQuery, /searchServices\(q:\s*\$q,\s*publicOnly:\s*true\)/);
  assert.match(catalogQuery, /\bis_public\b/);
});

test("notification mutations reject live Registry snapshots", () => {
  assert.equal(mutableWorkspaceNotificationID("engine:11111111-1111-1111-1111-111111111111"), "11111111-1111-1111-1111-111111111111");
  assert.equal(mutableWorkspaceNotificationID("11111111-1111-1111-1111-111111111111"), "11111111-1111-1111-1111-111111111111");
  assert.throws(
    () => mutableWorkspaceNotificationID("registry:11111111-1111-1111-1111-111111111111"),
    /cannot be updated/
  );
});
