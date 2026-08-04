import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { fileURLToPath } from "node:url";

import { artifactActivityIssue } from "./artifact-activity-error.ts";

const appRoutePath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks.$id.tsx"));
const mcpRoutePath = fileURLToPath(import.meta.resolve("../routes/integrations.mcp_.$id.analytics.tsx"));
const serviceActivityPath = fileURLToPath(import.meta.resolve("../components/AnalyticsTab.tsx"));
const nestedTabsPath = fileURLToPath(import.meta.resolve("../components/activity/NestedActivityTabs.tsx"));
const appIndexPath = fileURLToPath(import.meta.resolve("../routes/integrations.sdks._index.tsx"));
const runtimeStatusPath = fileURLToPath(import.meta.resolve("../components/artifacts/ArtifactRuntimeStatus.tsx"));

test("translates an inactive artifact scope without exposing an internal store error", () => {
  assert.deepEqual(artifactActivityIssue(new Error("sdk scope not found"), "sdk"), {
    message: "This app is not active on this Engine, so local execution activity is unavailable.",
    tone: "neutral",
  });
  assert.deepEqual(artifactActivityIssue(new Error("sdk scope not found"), "mcp"), {
    message: "This MCP server is not active on this Engine, so local execution activity is unavailable.",
    tone: "neutral",
  });
  assert.equal(artifactActivityIssue(new Error("database unavailable"), "sdk").tone, "error");
});

test("uses one subordinate underline treatment for nested activity views", async () => {
  const [appRoute, mcpRoute, serviceActivity, nestedTabs] = await Promise.all([
    readFile(appRoutePath, "utf8"),
    readFile(mcpRoutePath, "utf8"),
    readFile(serviceActivityPath, "utf8"),
    readFile(nestedTabsPath, "utf8"),
  ]);

  assert.match(appRoute, /<NestedActivityTabs/);
  assert.match(mcpRoute, /<NestedActivityTabs/);
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

test("reads app catalogue and details from Engine-local artifact snapshots", async () => {
  const [appIndex, appRoute, runtimeStatus] = await Promise.all([
    readFile(appIndexPath, "utf8"),
    readFile(appRoutePath, "utf8"),
    readFile(runtimeStatusPath, "utf8"),
  ]);
  assert.match(appIndex, /api\.mcpGraphql<\{ artifactSnapshots:/);
  assert.doesNotMatch(appIndex, /sdks\(limit: 100/);
  assert.match(appIndex, /<ArtifactRuntimeStatus className="mt-0\.5" active=\{sdk\.active\} runtimeState=\{sdk\.runtime_state\}/);
  assert.doesNotMatch(appIndex, /Needs configuration/);
  assert.match(appRoute, /artifactSnapshot\(id:/);
  assert.match(appRoute, /<ArtifactRuntimeStatus className="mt-1\.5" active=\{sdk\.active\} runtimeState=\{sdk\.runtime_state\}/);
  assert.doesNotMatch(appRoute, /sdk\(id:/);
  assert.match(runtimeStatus, /runtimeState === "needs_configuration"/);
  assert.match(runtimeStatus, /Setup required/);
});
