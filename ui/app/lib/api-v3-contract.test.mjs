import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { mutableWorkspaceNotificationID } from "./workspace-notification.ts";

const apiPath = fileURLToPath(import.meta.resolve("./api.ts"));
const integrationsIndexPath = fileURLToPath(import.meta.resolve("../routes/integrations._index.tsx"));
const integrationsListPath = fileURLToPath(import.meta.resolve("../components/IntegrationsListTab.tsx"));
const serviceIconPath = fileURLToPath(import.meta.resolve("../components/ServiceIcon.tsx"));
const integrationDetailPath = fileURLToPath(import.meta.resolve("../routes/integrations.$id.tsx"));

// sourceSection isolates one API contract so unrelated fields cannot satisfy assertions.
function sourceSection(source, start, end) {
  const startIndex = source.indexOf(start);
  const endIndex = source.indexOf(end, startIndex + start.length);
  assert.notEqual(startIndex, -1, `missing section start: ${start}`);
  assert.notEqual(endIndex, -1, `missing section end: ${end}`);
  return source.slice(startIndex, endIndex);
}

// Membership projections carry exact versions and the Registry metadata needed by workspace cards.
test("workspace membership uses exact enabled versions without a false workspace field", async () => {
  const source = await readFile(apiPath, "utf8");
  const section = sourceSection(source, "getServices: () =>", "listWebhookEvents:");

  assert.match(section, /enabled_versions\s*\{[^}]*service_version_id/s);
  for (const field of ["registry_slug", "description", "icon_url", "provider", "is_owner", "is_public"]) {
    assert.match(section, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(section, /\bworkspace_id\b/);
});

// The Services route must keep Workspace authoritative while loading optional catalogue markers from lightweight membership.
test("services route combines paged workspace rows with optional catalogue discovery", async () => {
  const [apiSource, routeSource, detailSource] = await Promise.all([
    readFile(apiPath, "utf8"),
    readFile(integrationsIndexPath, "utf8"),
    readFile(integrationDetailPath, "utf8"),
  ]);
  const membershipSection = sourceSection(apiSource, "getServiceIds: () =>", "upsertBucketValue:");
  const catalogueLoader = sourceSection(routeSource, "const loadCatalogData", "const loadWorkspaceData");
  const workspaceLoader = sourceSection(routeSource, "const loadWorkspaceData", "const loadVisibleData");
  const detailMembership = sourceSection(detailSource, "const [workspaceServiceActive", "const [drift");

  assert.match(membershipSection, /workspaceServiceIds/);
  assert.doesNotMatch(membershipSection, /auth_options|enabled_versions/);
  assert.match(catalogueLoader, /getServiceIds\(\)/);
  assert.doesNotMatch(catalogueLoader, /getServices\(\)/);
  assert.match(workspaceLoader, /getServicesPage\(10,/);
  assert.doesNotMatch(workspaceLoader, /getServices\(\)/);
  assert.match(workspaceLoader, /getServiceIds\(\)/);
  assert.match(workspaceLoader, /catalogVisible\(isAuth, catalogPreference, workspaceServiceIds\)/);
  assert.match(workspaceLoader, /loadCatalogData\(\{ knownWorkspaceServiceIds: workspaceServiceIds/);
  assert.doesNotMatch(routeSource, /data-track="view_catalog_tab"/);
  assert.match(routeSource, /role="switch"/);
  assert.match(routeSource, /Show catalog/);
  assert.match(detailMembership, /getServiceIds\(\)/);
  assert.doesNotMatch(detailMembership, /getServices\(\)/);
  assert.match(detailMembership, /catch[\s\S]*setWorkspaceServiceActive\(null\)/);
});

// Workspace activation must settle visibly and cannot turn repeated clicks into concurrent snapshot writes.
test("service add actions expose bounded single-flight feedback", async () => {
  const [apiSource, routeSource, listSource, detailSource] = await Promise.all([
    readFile(apiPath, "utf8"),
    readFile(integrationsIndexPath, "utf8"),
    readFile(integrationsListPath, "utf8"),
    readFile(integrationDetailPath, "utf8"),
  ]);
  const apiAdd = sourceSection(apiSource, "addService: (", "removeService:");
  const routeAdd = sourceSection(routeSource, "async function handleAddWorkspace", "const workspaceList");
  const cardAction = sourceSection(listSource, "function IntegrationActions", "function canAddCatalogIntegration");
  const detailAction = sourceSection(detailSource, "function AddToWorkspaceButton", "// useIntegrationDetailModel");

  assert.match(apiAdd, /reqWithTimeout/);
  assert.match(apiAdd, /50_000/);
  assert.match(routeAdd, /pendingWorkspaceServiceIdsRef\.current\.has\(id\)/);
  assert.match(routeAdd, /setActiveWorkspaceServiceIds/);
  assert.doesNotMatch(routeAdd, /loadVisibleData/);
  assert.match(cardAction, /aria-busy=\{pending\}/);
  assert.match(cardAction, /Loader2[\s\S]*animate-spin/);
  assert.match(detailAction, /aria-busy=\{adding\}/);
  assert.match(detailAction, /Loader2[\s\S]*animate-spin/);
});

// Catalogue rendering must expose useful service context without introducing per-card network reads.
test("services render as descriptive catalogue cards", async () => {
  const [routeSource, listSource, iconSource, detailSource] = await Promise.all([
    readFile(integrationsIndexPath, "utf8"),
    readFile(integrationsListPath, "utf8"),
    readFile(serviceIconPath, "utf8"),
    readFile(integrationDetailPath, "utf8"),
  ]);

  assert.match(routeSource, /id name description icon_url endpoint_count webhook_count base_url/);
  assert.match(listSource, /function IntegrationCard/);
  assert.match(listSource, /grid grid-cols-1 items-start gap-4 md:grid-cols-2 xl:grid-cols-3/);
  assert.match(listSource, /hover:border-black/);
  assert.doesNotMatch(listSource, /min-h-56/);
  assert.match(listSource, /<IntegrationDescription description=\{service\.description\}/);
  assert.doesNotMatch(listSource, /line-clamp-3/);
  assert.match(listSource, /<ServiceAnalytics endpointCount=\{service\.endpoint_count\} webhookCount=\{service\.webhook_count\}/);
  assert.match(listSource, /title=\{apiURL\} className="mt-1 break-all font-mono/);
  assert.doesNotMatch(listSource, /function DefaultAPIURL/);
  assert.match(listSource, /explicitDefault \|\| production \|\| service\.servers\[0\]/);
  assert.match(listSource, /if \(!content\) return null/);
  assert.doesNotMatch(listSource, /No description provided\./);
  assert.match(listSource, /service\.is_owner === false/);
  assert.match(listSource, />@\{providerName\}<\/p>/);
  assert.doesNotMatch(listSource, />By \{providerName\}<\/p>/);
  assert.match(listSource, /service_slug: s\.registry_slug \|\| s\.service_slug/);
  assert.match(listSource, /<ServiceVisibilityBadge isPublic=\{service\.is_public\}/);
  assert.match(listSource, /<ServiceIcon name=\{service\.name\} iconURL=\{service\.icon_url\}/);
  assert.match(iconSource, /referrerPolicy="no-referrer"/);
  assert.match(iconSource, /onError=\{\(\) => setFailed\(true\)\}/);
  assert.match(detailSource, /<ServiceIcon name=\{srv\.name\} iconURL=\{srv\.icon_url\}/);
  assert.match(listSource, /className="block truncate text-sm font-semibold/);
  assert.doesNotMatch(listSource, /Service integration/);
  assert.doesNotMatch(listSource, /function WorkspaceIntegrationAction/);
  assert.match(listSource, /In workspace/);
});

// Workspace removal belongs on a context-rich detail page and must never delete the Registry definition.
test("service removal is available only from service details", async () => {
  const [listSource, detailSource] = await Promise.all([
    readFile(integrationsListPath, "utf8"),
    readFile(integrationDetailPath, "utf8"),
  ]);

  assert.doesNotMatch(listSource, /data-track="remove_workspace_service"/);
  assert.match(detailSource, /data-track="remove_workspace_service"/);
  assert.match(detailSource, /aria-label="Service actions"/);
  assert.match(detailSource, /role="menuitem"/);
  assert.match(detailSource, /createPortal\(<WorkspaceMembershipControl srv=\{srv\} \/>, host\)/);
  assert.match(detailSource, /api\.workspace\.removeService\(serviceId\)/);
  assert.match(detailSource, /setWorkspaceServiceActive\(false\)/);
  assert.doesNotMatch(detailSource, /api\.integrations\.delete/);
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
  const importsAPI = sourceSection(apiSource, "integrations: {", "startDiscovery: (");

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
