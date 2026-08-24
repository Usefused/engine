import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const serviceActivity = readFileSync(new URL("../components/AnalyticsTab.tsx", import.meta.url), "utf8");
const appRequests = readFileSync(new URL("../components/activity/AppRequestsPanel.tsx", import.meta.url), "utf8");
const sdkDetails = readFileSync(new URL("../routes/integrations.sdks.$id.tsx", import.meta.url), "utf8");
const mcpActivity = readFileSync(new URL("../routes/integrations.mcp_.$id.analytics.tsx", import.meta.url), "utf8");
const sharedDrawer = readFileSync(new URL("../components/activity/ExecutionDetailsDrawer.tsx", import.meta.url), "utf8");
const webhookLogs = readFileSync(new URL("../components/webhooks/WebhookLogsCard.tsx", import.meta.url), "utf8");
const webhookDrawer = readFileSync(new URL("../components/webhooks/WebhookEventDetailsDrawer.tsx", import.meta.url), "utf8");

// Confirms App and MCP receipt rows open the same wide canonical detail drawer.
test("app receipt rows open shared execution details", () => {
  assert.match(appRequests, /role="button" tabIndex=\{0\} aria-haspopup="dialog"/);
  assert.match(appRequests, /onSelect=\{setSelectedEvent\}/);
  assert.match(appRequests, /<ExecutionDetailsDrawer event=\{selectedEvent\}/);
  assert.match(appRequests, /<ExecutionDetails event=\{selectedEvent\}/);
  assert.match(appRequests, /new Map\(\[\[appId, consumerName\]\]\)/);
  assert.match(sdkDetails, /consumerName=\{sdk\.name\}/);
  assert.match(mcpActivity, /consumerName=\{serverName\}/);
  assert.match(sharedDrawer, /xl:max-w-\[1080px\]/);
});

// Confirms Service outbound rows use the shared drawer instead of inline expansion.
test("service receipt rows open shared execution details", () => {
  assert.match(serviceActivity, /aria-label=\{`Inspect \$\{event\.operation\}`\}/);
  assert.match(serviceActivity, /<ExecutionDetailsDrawer event=\{selectedExecution\}/);
  assert.doesNotMatch(serviceActivity, /SelectedExecutionPanel/);
});

// Confirms mobile service receipts stack metadata without introducing a wide table or clipped row target.
test("service receipt activity remains bounded and fully clickable on mobile", () => {
  assert.match(serviceActivity, /divide-y divide-slate-100 md:hidden/);
  assert.match(serviceActivity, /group block w-full min-w-0 p-4 text-left/);
  assert.match(serviceActivity, /flex flex-wrap items-center justify-between/);
  assert.match(serviceActivity, /line-clamp-2 break-all/);
});

// Confirms the shared inspector preserves its wide desktop size while confining mobile scrolling to the viewport.
test("execution details drawer is viewport safe on mobile", () => {
  assert.match(sharedDrawer, /h-dvh max-h-dvh w-full max-w-full/);
  assert.match(sharedDrawer, /flex-1 overflow-y-auto overflow-x-hidden overscroll-contain/);
  assert.match(sharedDrawer, /\[overflow-wrap:anywhere\]/);
  assert.match(sharedDrawer, /xl:max-w-\[1080px\]/);
});

// Confirms incoming webhook cards and rows expose only the bounded reduced receipt.
test("webhook receipt rows open bounded webhook details", () => {
  assert.match(webhookLogs, /<WebhookEventCard[^>]+onSelect=\{setSelectedEventID\}/);
  assert.match(webhookLogs, /<WebhookEventRow[^>]+onSelect=\{setSelectedEventID\}/);
  assert.match(webhookLogs, /<WebhookEventDetailsDrawer event=\{selectedEvent\}/);
  for (const label of ["Event", "Verification", "Delivery", "Environment", "Delivery time", "Payload size", "Retries", "Received", "Message ID", "Receipt ID"]) {
    assert.match(webhookDrawer, new RegExp(`label="${label}"`));
  }
  assert.doesNotMatch(webhookDrawer, /event\.(?:error_reason|payload(?!_size)|credentials?|secrets?)/i);
});
