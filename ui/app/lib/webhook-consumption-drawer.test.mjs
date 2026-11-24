import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { JSDOM } from "jsdom";
import ts from "typescript";
import * as workspaceDialogHost from "./workspace-dialog-host.ts";
import * as formatHelpers from "./format.ts";
import * as webhookDetailHelpers from "./webhook-detail.ts";

const require = createRequire(import.meta.url);
const drawerSource = readFileSync(new URL("../components/webhooks/WebhookEventDetailsDrawer.tsx", import.meta.url), "utf8");

/** Transpiles the production drawer component while isolating unrelated disclosure and agent context dependencies. */
function loadDrawer() {
  const code = ts.transpileModule(drawerSource, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
  const exports = {};
  /** Resolves real React/portal dependencies while stubbing only context/sibling inspectors. */
  function resolve(name) {
    if (name === "~/components/agent/FusedAgentContext") return { useFusedAgent: () => ({ isOpen: false }) };
    if (name === "./WebhookMessageDiagnostics") return { WebhookMessageDiagnostics: () => null };
    if (name === "~/components/OperationDetailsParts") return { OperationDisclosure: ({ label, children }) => React.createElement("section", null, label, children) };
    if (name === "~/lib/workspace-dialog-host") return workspaceDialogHost;
    if (name === "~/lib/format") return formatHelpers;
    if (name === "~/lib/webhook-detail") return webhookDetailHelpers;
    return require(name);
  }
  new Function("require", "exports", code)(resolve, exports);
  return exports.WebhookEventDetailsDrawer;
}

/** Builds a baseline webhook receipt entry with overridable delivery and consumption fields. */
function baseEvent(overrides = {}) {
  return {
    id: "receipt-1",
    account_id: "account-1",
    service_id: "service-1",
    registration_id: "registration-1",
    msg_id: "msg-1",
    event_name: "checkout.session.completed",
    status: "ingested",
    delivery_status: "ingested",
    verification_status: "verified",
    execution_status: "success",
    latency_ms: 0,
    retry_count: 0,
    attempt_count: 1,
    credits_consumed: 0,
    error_reason: "",
    payload_size: 3600,
    created_at: "2026-11-17T11:17:35Z",
    ended_at: "2026-11-17T11:17:35Z",
    recorded_at: "2026-11-17T11:17:35Z",
    consumptions: [],
    ...overrides,
  };
}

// Verifies portal isolation outside @container and accurate consumer + timestamp states in the receipt drawer.
test("webhook details drawer portals into workspace host and displays consumers and consumption timestamps", async () => {
  const dom = new JSDOM('<!doctype html><div id="workspace"><div class="@container" id="card-root"></div><div id="fused-workspace-dialogs"></div></div>');
  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.HTMLElement = dom.window.HTMLElement;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;

  const Drawer = loadDrawer();
  const cardRoot = document.getElementById("card-root");
  const dialogHost = document.getElementById("fused-workspace-dialogs");
  const root = createRoot(cardRoot);

  try {
    // Unconsumed ingested receipt clearly reports no consumer and portals out of the @container subtree.
    await act(async () => {
      root.render(React.createElement(Drawer, { event: baseEvent(), serviceName: "@martins-joseph/stripe", onClose: () => {} }));
    });
    assert.equal(cardRoot.querySelector("[data-fused-detail-sidebar]"), null);
    assert.ok(dialogHost.querySelector("[data-fused-detail-sidebar]"));
    assert.match(dialogHost.textContent, /Consumed byNo one/);
    assert.match(dialogHost.textContent, /Consumed atNot consumed/);
    assert.match(dialogHost.textContent, /No one has consumed this webhook yet\./);

    // Rejected UNKNOWN receipt explains why no consumer received it and avoids raw UNKNOWN copy.
    await act(async () => {
      root.render(React.createElement(Drawer, { event: baseEvent({ event_name: "UNKNOWN", status: "rejected", delivery_status: "rejected", verification_status: "rejected" }), serviceName: "@martins-joseph/stripe", onClose: () => {} }));
    });
    assert.match(dialogHost.textContent, /Unverified event/);
    assert.doesNotMatch(dialogHost.textContent, /UNKNOWN/);
    assert.match(dialogHost.textContent, /Not consumed — this webhook was rejected during verification\./);

    // Single SDK consumer displays transport badge, display name, version, receiver, and timestamp.
    await act(async () => {
      root.render(React.createElement(Drawer, {
        event: baseEvent({
          status: "success",
          delivery_status: "success",
          consumptions: [{
            consumer_kind: "sdk",
            consumer_name: "Billing SDK",
            app_family_id: "fam-sdk",
            app_id: "app-sdk",
            app_version: "1.2.0",
            receiver_name: "primary-worker",
            consumed_at: "2026-11-20T16:46:20Z",
          }],
        }),
        serviceName: "@martins-joseph/stripe",
        onClose: () => {},
      }));
    });
    assert.match(dialogHost.textContent, /Consumed bySDK · Billing SDK/);
    assert.match(dialogHost.textContent, /v1\.2\.0/);
    assert.match(dialogHost.textContent, /Receiver: primary-worker/);
    assert.equal(dialogHost.querySelectorAll('time[datetime="2026-11-20T16:46:20Z"]').length, 1);

    // Mixed consumers (SDK, MCP, Unified App) all appear with their respective labels and timestamps.
    await act(async () => {
      root.render(React.createElement(Drawer, {
        event: baseEvent({
          status: "success",
          delivery_status: "success",
          consumptions: [
            { consumer_kind: "sdk", consumer_name: "Billing SDK", app_id: "app-sdk", consumed_at: "2026-11-20T16:46:20Z" },
            { consumer_kind: "mcp", consumer_name: "Ops MCP", app_id: "app-mcp", consumed_at: "2026-11-20T16:47:00Z" },
            { consumer_kind: "unified_app", consumer_name: "Order Sync", app_family_id: "fam-ua", consumed_at: "2026-11-20T16:48:00Z" },
          ],
        }),
        serviceName: "@martins-joseph/stripe",
        onClose: () => {},
      }));
    });
    assert.match(dialogHost.textContent, /3 consumers \(SDK, MCP, Unified App\)/);
    assert.match(dialogHost.textContent, /Billing SDK/);
    assert.match(dialogHost.textContent, /Ops MCP/);
    assert.match(dialogHost.textContent, /Order Sync/);
    assert.equal(dialogHost.querySelectorAll('section[aria-label="Webhook consumption"] li').length, 3);

    // Historical delivered receipt without consumer records avoids falsely claiming nobody consumed it, and formats multi-day delivery latency cleanly.
    await act(async () => {
      root.render(React.createElement(Drawer, {
        event: baseEvent({ status: "success", delivery_status: "success", latency_ms: 278_925_548, ended_at: "2026-11-20T16:46:20Z", consumptions: [] }),
        serviceName: "@martins-joseph/stripe",
        onClose: () => {},
      }));
    });
    assert.match(dialogHost.textContent, /Consumed byConsumer not recorded/);
    assert.match(dialogHost.textContent, /Delivery time3d 5h/);
    assert.doesNotMatch(dialogHost.textContent, /278925548 ms/);
    assert.match(dialogHost.textContent, /consumer details were not recorded for this historical receipt/);
  } finally {
    await act(async () => root.unmount());
    dom.window.close();
    delete globalThis.window;
    delete globalThis.document;
    delete globalThis.HTMLElement;
    delete globalThis.IS_REACT_ACT_ENVIRONMENT;
  }
});
