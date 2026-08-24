import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { appActivityIssue } from "./app-activity-error.ts";

const appRoutePath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks.$id.tsx"));
const mcpActivityPath = fileURLToPath(import.meta.resolve("../routes/integrations.mcp_.$id.analytics.tsx"));
const mcpDetailPath = fileURLToPath(import.meta.resolve("../routes/integrations.mcp_.$id.tsx"));
const serviceActivityPath = fileURLToPath(import.meta.resolve("../components/AnalyticsTab.tsx"));
const nestedTabsPath = fileURLToPath(import.meta.resolve("../components/activity/NestedActivityTabs.tsx"));
const appIndexPath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks._index.tsx"));
const runtimeStatusPath = fileURLToPath(import.meta.resolve("../components/apps/AppRuntimeStatus.tsx"));
const apiPath = fileURLToPath(import.meta.resolve("./api.ts"));
const appBuilderPath = fileURLToPath(import.meta.resolve("../routes/integrations.builder.tsx"));
const appOverviewPath = fileURLToPath(import.meta.resolve("../components/activity/AppActivityOverview.tsx"));
const appRequestsPath = fileURLToPath(import.meta.resolve("../components/activity/AppRequestsPanel.tsx"));

test("translates a missing local app without exposing an internal store error", () => {
  assert.deepEqual(appActivityIssue(new Error("app not found"), "sdk"), {
    message: "This app is not active on this Engine, so local execution activity is unavailable.",
    tone: "neutral",
  });
  assert.deepEqual(appActivityIssue(new Error("app not found"), "mcp"), {
    message: "This MCP server is not active on this Engine, so local execution activity is unavailable.",
    tone: "neutral",
  });
  assert.equal(appActivityIssue(new Error("database unavailable"), "sdk").tone, "error");
});

test("uses one subordinate underline treatment for nested activity views", async () => {
  const [appRoute, mcpActivity, serviceActivity, nestedTabs] = await Promise.all([
    readFile(appRoutePath, "utf8"),
    readFile(mcpActivityPath, "utf8"),
    readFile(serviceActivityPath, "utf8"),
    readFile(nestedTabsPath, "utf8"),
  ]);

  assert.match(appRoute, /<NestedActivityTabs/);
  assert.match(mcpActivity, /<NestedActivityTabs/);
  assert.match(serviceActivity, /<NestedActivityTabs/);
  assert.match(nestedTabs, /border-b-2/);
  assert.doesNotMatch(serviceActivity, />This Engine</);
  assert.doesNotMatch(serviceActivity, />Across Fused Engines</);
});

test("keeps aggregate service usage inside Outbound calls as a select option", async () => {
  const serviceActivity = await readFile(serviceActivityPath, "utf8");

  assert.match(serviceActivity, /aria-label="Outbound analytics source"/);
  assert.match(serviceActivity, /<option value="local">Workspace calls<\/option>/);
  assert.match(serviceActivity, /<option value="cross-engine">Aggregate usage<\/option>/);
  assert.match(serviceActivity, /No other Fused users have used this service in the last 30 days\./);
  assert.doesNotMatch(serviceActivity, /No cross-engine usage has been reported/);
});

test("reads exact app versions and family state from the Engine catalogue", async () => {
  const [appIndex, appRoute, mcpDetail, mcpActivity, runtimeStatus] = await Promise.all([
    readFile(appIndexPath, "utf8"),
    readFile(appRoutePath, "utf8"),
    readFile(mcpDetailPath, "utf8"),
    readFile(mcpActivityPath, "utf8"),
    readFile(runtimeStatusPath, "utf8"),
  ]);
  assert.match(appIndex, /\.mcpGraphql<\{ apps: SdkPage/);
  assert.match(appIndex, /app_id/);
  assert.match(appIndex, /app_family_id/);
  assert.match(appIndex, /status downloads/);
  assert.match(appIndex, /await fetchSdks\(query, page\)/);
  assert.match(appIndex, /query SDKApps\(\$search: String!, \$version: String!, \$limit: Int!, \$offset: Int!\)/);
  assert.match(appIndex, /<SdkPagination page=\{page\} total=\{total\}/);
  assert.doesNotMatch(appIndex, /artifactSnapshots/);
  assert.match(appIndex, /<AppRuntimeStatus className="mt-0\.5" status=\{sdk\.status\}/);
  assert.match(appRoute, /app\(app_id:/);
  assert.match(appRoute, /appVersions\(app_family_id:/);
  assert.match(appRoute, /appServices\(app_id:/);
  assert.match(appRoute, /schema_version/);
  assert.match(appRoute, /status\s+downloads/);
  assert.match(appRoute, /await fetchSdk\(sdk\.app_id\)/);
  assert.match(appRoute, /appConnectedServiceSelections\(res\.app\.selections, res\.appServices\)/);
  assert.match(mcpDetail, /app\(app_id: \$appId\)/);
  assert.match(mcpDetail, /appServices\(app_id: \$appId\)/);
  assert.match(mcpDetail, /appVersions\(app_family_id: \$appFamilyId\)/);
  assert.match(mcpDetail, /operation_names/);
  assert.match(mcpDetail, /appConnectedServiceSelections\(result\.app\.selections, result\.appServices\)/);
  assert.match(mcpDetail, /result\.app\.kind !== "mcp"/);
  assert.match(mcpActivity, /hasResourcePermission\(access, "app\.read", "APP", appFamilyId\)/);
  assert.doesNotMatch(appRoute, /artifactSnapshot|sdkAnalytics|sdkSelectionResources/);
  assert.match(runtimeStatus, /status === "deprecated"/);
  assert.match(runtimeStatus, /Deprecated/);
});

test("uses only exact Engine app lifecycle and package routes", async () => {
	const [appIndex, api, appBuilder] = await Promise.all([
		readFile(appIndexPath, "utf8"),
		readFile(apiPath, "utf8"),
		readFile(appBuilderPath, "utf8"),
	]);

	assert.ok(api.includes("`${BASE}/sdks/${id}/download`"));
	assert.ok(api.includes('deactivate: (appId: string) => req<void>(`/apps/${appId}/`'));
	assert.doesNotMatch(api, /upgrade_from|upgradeAsync|generateAsync|\/sdk-config\/\$\{id\}/);
	assert.doesNotMatch(appIndex, /Upgrade SDK|onUpgrade|api\.sdks\.delete/);
	assert.doesNotMatch(appBuilder, /upgrade_from|upgradeFrom|lockedSelections|lockedWebhookSelections/);
});

test("groups generated SDK and direct REST receipts under the same app", async () => {
	const [overview, requests, api] = await Promise.all([
		readFile(appOverviewPath, "utf8"),
		readFile(appRequestsPath, "utf8"),
		readFile(apiPath, "utf8"),
	]);
	assert.match(overview, /getAppExecutionAnalytics\(\{ appId, includeAllVersions \}\)/);
	assert.match(requests, /transport: transport === "mcp" \? "mcp" : undefined/);
	assert.match(api, /transport: "sdk" \| "mcp" \| "rest" \| "webhook"/);
});
