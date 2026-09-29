import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";
import * as helpers from "./unified-receipt.ts";
import * as traceHelpers from "./unified-app-trace.ts";
import * as outcomeHelpers from "./unified-app-outcome.ts";

const require = createRequire(import.meta.url);

// source reads only the local implementation so contract checks do not need a live Engine or personal receipts.
function source(path) {
  return readFileSync(new URL(path, import.meta.url), "utf8");
}

// loadDetails compiles the real receipt and trace JSX so ordinary rendering has no API or diagnostics dependency.
function loadDetails(path = "../components/activity/UnifiedExecutionDetails.tsx") {
  const output = ts.transpileModule(source(path), {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
  const exports = {};
  // App aliases use real local implementations; framework imports keep their installed behavior.
  const resolve = (name) => {
    const local = { "~/lib/unified-receipt": helpers, "~/lib/unified-app-trace": traceHelpers, "~/lib/unified-app-outcome": outcomeHelpers };
    // Nested trace rendering exercises the actual failure marker, not a placeholder component.
    if (name === "~/components/activity/UnifiedAppTrace") return loadDetails("../components/activity/UnifiedAppTrace.tsx");
    return local[name] ?? require(name);
  };
  new Function("require", "exports", output)(resolve, exports);
  return exports;
}

const { UnifiedExecutionDetails } = loadDetails();

// parentFixture models bounded orchestration evidence, including a skipped step and compensation of a completed step.
function parentFixture(overrides = {}) {
  return {
    id: "parent", execution_kind: "unified", operation: "sample.synchronize", transport: "mcp", status: "failed",
    latency_ms: 75, started_at: "2026-08-26T09:00:00Z", failure_code: "unified_target_failed",
    unified_steps: [
      { target: "first", phase: "forward", status: "success" },
      { target: "second", phase: "forward", status: "error", error_code: "input_mapping_failed" },
      { target: "third", phase: "forward", status: "skipped" },
      { target: "first", phase: "rollback", status: "success" },
    ], ...overrides,
  };
}

// childFixture intentionally gives both phases the same target to detect incorrect name-only joining.
function childFixture(phase, overrides = {}) {
  return { id: `child-${phase}`, parent_execution_id: "parent", unified_target: "first", execution_phase: phase,
    execution_kind: "physical", service_name: "Sample service", latency_ms: 50, ...overrides };
}

// renderParent exercises the real markup with synthetic metadata, including unknown fields that must stay private.
function renderParent(event, children = [], overrides = {}) {
  return renderToStaticMarkup(createElement(UnifiedExecutionDetails, {
    event, consumerName: "Sample app", rows: helpers.unifiedStepRows(event, children), loading: false,
    unavailable: false, onRetry: () => {}, onSelect: () => {}, ...overrides,
  }));
}

// The same target needs separate forward and compensation receipts, and unrelated parents must never link.
test("joins one child page by parent, target, and phase without guessing skipped receipts", () => {
  const forward = childFixture("forward");
  const rollback = childFixture("rollback");
  const foreign = childFixture("forward", { parent_execution_id: "another-parent", id: "foreign" });
  const rows = helpers.unifiedStepRows(parentFixture(), [forward, rollback, foreign, { id: "legacy" }]);
  assert.deepEqual(rows.map(({ receipt }) => receipt?.id), ["child-forward", undefined, undefined, "child-rollback"]);
  assert.notEqual(helpers.unifiedStepKey({ phase: "forward", target: "x,rollback" }), helpers.unifiedStepKey({ phase: "rollback", target: "x,forward" }));
});

// Successful rollback must not turn a failed logical call into a success or erase skipped targets.
test("counts logical outcomes independently from compensation and provider attempts", () => {
  const rows = helpers.unifiedStepRows(parentFixture(), []);
  assert.equal(helpers.unifiedPhaseSummary(rows, "forward"), "1 succeeded · 1 failed · 1 skipped");
  assert.equal(helpers.unifiedPhaseSummary(rows, "rollback"), "1 succeeded · 0 failed");
  assert.equal(helpers.unifiedStepStatus("skipped").label, "Skipped");
});

// Backward-compatible physical receipts still use the provider route while parents advertise their logical role.
test("request labels do not invent a provider route for Unified calls", () => {
  assert.equal(helpers.receiptRequestLabel(parentFixture()), "Unified operation");
  assert.equal(helpers.receiptRequestLabel({ http_method: "GET", request_path: "/items", direction: "outbound" }), "GET /items");
  assert.equal(helpers.receiptRequestLabel({ direction: "outbound" }), "outbound");
  assert.deepEqual(helpers.unifiedStepRows(parentFixture({ unified_steps: undefined }), []), []);
});

// Both transports render the same parent view and only genuinely stored physical executions become buttons.
test("renders SDK and MCP parent receipts with clickable forward and rollback evidence", () => {
  for (const transport of ["sdk", "mcp"]) {
    const html = renderParent(parentFixture({ transport }), [childFixture("forward"), childFixture("rollback")]);
    assert.match(html, /Unified execution receipt/);
    assert.match(html, new RegExp(`>${transport.toUpperCase()}<`));
    assert.match(html, /aria-label="Inspect forward execution first"/);
    assert.match(html, /aria-label="Inspect rollback execution first"/);
    assert.doesNotMatch(html, /aria-label="Inspect (?:forward|rollback) execution (?:second|third)"/);
    assert.match(html, /Compensates first/);
    assert.match(html, /Operation results are shown below/);
    assert.match(html, /75 ms/);
    assert.doesNotMatch(html, /Provider round trip|Engine work/);
  }
});

// The ordinary receipt remains useful with no diagnostics client and never renders accidental private fields.
test("ordinary app receipt explains a failed stage with version and provider outcomes", () => {
  const event = parentFixture({ app_version: "1.0.0", failure_reason: "unified_app_execute_failed", failure_code: "execution_failed",
    error: "PRIVATE_EXCEPTION", request: "PRIVATE_REQUEST", response: "PRIVATE_RESPONSE",
    timings: [{ name: "unified_app_execute", duration_ms: 40 }],
  });
  const html = renderParent(event, [childFixture("forward", { operation: "getCustomer", provider_http_status: 200, attempt_count: 1 })]);
  assert.match(html, /Failed · Run TypeScript/);
  assert.match(html, /stopped while running its code/);
  assert.match(html, /Execution trace/);
  assert.match(html, /bg-red-500/);
  assert.match(html, /1\.0\.0/);
  assert.match(html, /getCustomer/);
  assert.match(html, /HTTP 200/);
  assert.doesNotMatch(html, /PRIVATE_|Private diagnostics|Rollback executions/);
});

// Metadata is rendered as text and even accidental payload properties are excluded from this audit-only projection.
test("escapes target names and never renders personal payloads or credentials", () => {
  const event = parentFixture({ operation: "<sample&operation>", payload: "PRIVATE_PAYLOAD", credentials: "PRIVATE_SECRET",
    unified_steps: [{ target: "<target&value>", phase: "forward", status: "error", error_code: "input_mapping_failed" }] });
  const html = renderParent(event);
  assert.match(html, /&lt;sample&amp;operation&gt;/);
  assert.match(html, /&lt;target&amp;value&gt;/);
  assert.doesNotMatch(html, /PRIVATE_PAYLOAD|PRIVATE_SECRET|<target&value>/);
  assert.match(html, /Physical receipt not available/);
  assert.match(html, /No rollback executions recorded/);
});

// Eventual delivery and request failure remain visible and refresh only reads canonical receipt history.
test("renders honest loading, missing, and unavailable states", () => {
  const loading = renderParent(parentFixture(), [], { loading: true });
  assert.match(loading, /role="status"/);
  assert.match(loading, /Loading execution receipts/);
  assert.match(loading, /disabled=""/);
  const failed = renderParent(parentFixture(), [], { unavailable: true });
  assert.match(failed, /Child receipts are unavailable/);
  assert.match(failed, /Refresh receipts/);
});

// Source wiring asserts one scoped read and one drawer, avoiding both N+1 fetching and provider replay.
test("child navigation reuses canonical receipt data and restores parent position with reduced motion", () => {
  const inspector = source("../components/activity/AppExecutionInspector.tsx");
  const drawer = source("../components/activity/ExecutionDetailsDrawer.tsx");
  const css = source("../tailwind.css");
  const api = source("./api.ts");
  assert.equal((inspector.match(/api\.workspace\.listAppExecutionEvents\(/g) ?? []).length, 1);
  assert.match(inspector, /includeAllVersions: true, parentExecutionId: event.id, limit: 50/);
  assert.match(inspector, /if \(active\) setChildren/);
  assert.match(inspector, /parentScrollTop.current = scrollRef.current\?\.scrollTop/);
  assert.match(inspector, /restoreScrollTop=\{child \? 0 : parentScrollTop.current\}/);
  assert.match(inspector, /<ExecutionDetails event=\{selected\}/);
  assert.match(drawer, /Back to Unified/);
  assert.match(drawer, /scrollRef.current.scrollTop = restoreScrollTop/);
  assert.match(css, /@keyframes receipt-view-forward/);
  assert.match(css, /@keyframes receipt-view-backward/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)/);
  assert.match(css, /animation: none/);
  assert.match(api, /parent_execution_id: \$parentExecutionId/);
  assert.match(api, /unified_steps \{ target phase status error_code \}/);
  assert.doesNotMatch(inspector, /executeResolved|api\.[a-zA-Z.]*execute\(/);
});
