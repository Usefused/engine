import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const activityRoute = readFileSync(new URL("../routes/integrations.activity.tsx", import.meta.url), "utf8");
const activityPanel = readFileSync(new URL("../components/IntegrationsAnalyticsTab.tsx", import.meta.url), "utf8");
const notificationsPanel = readFileSync(new URL("../components/notifications/NotificationsContent.tsx", import.meta.url), "utf8");
const apiSource = readFileSync(new URL("./api.ts", import.meta.url), "utf8");

// Confirms fixed date presets become exact Engine aggregate query bounds.
test("workspace Activity supports bounded reporting windows", () => {
  assert.match(activityPanel, /"24h" \| "7d" \| "30d" \| "90d"/);
  assert.match(activityRoute, /useState<WorkspaceActivityRange>\("7d"\)/);
  assert.match(activityRoute, /startDate\.toISOString\(\), endDate: endDate\.toISOString\(\)/);
  assert.match(activityRoute, /getWorkspaceExecutionAnalytics\(workspaceActivityRange\(range\)\)/);
});

// Locks workspace Activity to aggregate data without a competing receipt list.
test("workspace Activity does not request or render execution receipts", () => {
  assert.doesNotMatch(activityRoute, /listWorkspaceExecutionEvents/);
  assert.doesNotMatch(apiSource, /workspaceExecutionEvents/);
  assert.doesNotMatch(activityPanel, /ExecutionDetails|Recent executions|receipt drawer/i);
});

// Verifies every requested workspace leader and inbound total comes from GraphQL.
test("workspace Activity renders inbound and leader aggregates", () => {
  for (const field of ["inbound_calls", "most_used_sdk", "most_used_service", "most_failed_service", "most_used_bucket"]) {
    assert.match(apiSource, new RegExp(field));
    assert.match(activityPanel, new RegExp(field));
  }
  assert.match(activityPanel, /Inbound traffic overview/);
  assert.match(activityPanel, /Service traffic/);
});

// Prevents an app-family aggregate from being routed as an exact app version.
test("most-used SDK remains informational until an exact app id is available", () => {
  assert.match(activityPanel, /label="Most-used SDK"[^\n]+detail=/);
  assert.doesNotMatch(activityPanel, /Most-used SDK"[^\n]+href=/);
});

// Protects the small-screen layouts from regressing into wide desktop rows.
test("workspace Activity panels use bounded mobile layouts", () => {
  assert.match(activityRoute, /grid-flow-col auto-cols-fr/);
  assert.match(activityPanel, /grid w-full gap-1\.5[^"]+sm:flex sm:w-auto/);
  assert.match(activityPanel, /grid grid-cols-2 gap-x-4 gap-y-3/);
  assert.match(activityPanel, /sm:grid-cols-\[minmax\(0,1fr\)_repeat\(4,auto\)\]/);
  assert.match(notificationsPanel, /grid w-full grid-cols-3 gap-1/);
  assert.match(notificationsPanel, /hidden items-center gap-1 sm:flex/);
  assert.match(notificationsPanel, /min-\[380px\]:flex-row/);
});
