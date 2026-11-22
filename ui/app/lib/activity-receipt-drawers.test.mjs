import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

const serviceActivity = readFileSync(new URL("../components/AnalyticsTab.tsx", import.meta.url), "utf8");
const appRequests = readFileSync(new URL("../components/activity/AppRequestsPanel.tsx", import.meta.url), "utf8");
const sdkDetails = readFileSync(new URL("../routes/integrations.sdks.$id.tsx", import.meta.url), "utf8");
const mcpActivity = readFileSync(new URL("../routes/integrations.mcp_.$id.analytics.tsx", import.meta.url), "utf8");
const sharedDrawer = readFileSync(new URL("../components/activity/ExecutionDetailsDrawer.tsx", import.meta.url), "utf8");
const appInspector = readFileSync(new URL("../components/activity/AppExecutionInspector.tsx", import.meta.url), "utf8");
const webhookLogs = readFileSync(new URL("../components/webhooks/WebhookLogsCard.tsx", import.meta.url), "utf8");
const webhookDrawer = readFileSync(new URL("../components/webhooks/WebhookEventDetailsDrawer.tsx", import.meta.url), "utf8");
const agentProvider = readFileSync(new URL("../components/agent/FusedAgentProvider.tsx", import.meta.url), "utf8");
const integrationsLayout = readFileSync(new URL("../routes/integrations.tsx", import.meta.url), "utf8");
const integrationsSidebar = readFileSync(new URL("../components/layout/IntegrationsSidebar.tsx", import.meta.url), "utf8");

// Confirms App and MCP receipt rows open the same wide canonical detail drawer.
test("app receipt rows open shared execution details", () => {
  assert.match(appRequests, /role="button" tabIndex=\{0\} aria-haspopup="dialog"/);
  assert.match(appRequests, /onSelect=\{setSelectedEvent\}/);
  assert.match(appRequests, /<AppExecutionInspector key=\{selectedEvent.id\} event=\{selectedEvent\}/);
  assert.match(appInspector, /<ExecutionDetailsDrawer event=\{selected\}/);
  assert.match(appInspector, /<ExecutionDetails event=\{selected\}/);
  // Historical receipts use their own immutable version ID, with the current app only as a missing-ID fallback.
  assert.match(appInspector, /new Map\(\[\[selected\.app_id \|\| appId, consumerName\]\]\)/);
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

// Confirms canonical failure reasons and consumption history remain inspectable while provider bodies and credentials stay excluded.
test("webhook receipt rows open bounded webhook details", () => {
  assert.match(webhookLogs, /<WebhookReceiptRow[^>]+onSelect=\{setSelectedEventID\}/);
  assert.match(webhookLogs, /<button type="button" aria-haspopup="dialog"/);
  assert.doesNotMatch(webhookLogs, /<table|event\.environment/);
  assert.match(webhookLogs, /<WebhookEventDetailsDrawer event=\{selectedEvent\}/);
  // Keep the fixed drawer outside the @container box so container containment cannot trap fixed positioning.
  assert.match(webhookLogs, /<\/div>\s*\{\/\*[^*]*\*\/\}\s*\{selectedEvent && <WebhookEventDetailsDrawer/);
  for (const label of ["Event", "Verification", "Delivery", "Service", "Consumed by", "Consumed at", "Delivery time", "Payload size", "Retries", "Received", "Message ID", "Receipt ID"]) {
    assert.match(webhookDrawer, new RegExp(`label="${label}"`));
  }
  assert.match(webhookDrawer, /createPortal\(drawer, workspaceDialogHost\(\)\)/);
  assert.match(webhookDrawer, /h-dvh max-h-dvh w-full max-w-full/);
  assert.match(webhookDrawer, /overscroll-contain/);
  assert.match(webhookDrawer, /Failure reason/);
  assert.match(webhookDrawer, /event\.error_reason/);
  assert.doesNotMatch(webhookDrawer, /event\.(?:payload(?!_size)|credentials?|secrets?)/i);
});

// Confirms absolute descendants like sr-only cannot escape the h-dvh shell and create a root document scrollbar.
test("workspace shell contains absolute descendants so fixed overlays cannot scroll past navigation", () => {
  // Both the h-dvh shell and workspace scroller must be positioned without transform so absolute nodes stay clipped while fixed drawers stay viewport-anchored.
  assert.match(agentProvider, /className="relative flex h-dvh min-w-0 flex-col overflow-hidden overscroll-none md:flex-row"/);
  assert.match(agentProvider, /data-fused-workspace-pane className="relative isolate min-h-0 min-w-0 flex-1 overflow-auto overscroll-contain"/);
  assert.doesNotMatch(agentProvider, /data-fused-workspace-pane[^>]*transform/);
  // Workspace root and sticky rail must align to the scroller's dynamic viewport height and contain absolute row labels.
  assert.match(integrationsLayout, /data-fused-workspace className="relative min-h-full flex flex-col lg:flex-row/);
  assert.match(integrationsSidebar, /sticky top-0 h-dvh shrink-0/);
  assert.match(webhookLogs, /<button type="button" aria-haspopup="dialog" onClick=\{\(\) => onSelect\(event\.id\)\} className=\{`relative grid /);
});
