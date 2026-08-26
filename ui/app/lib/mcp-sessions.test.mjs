import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import test from "node:test";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import ts from "typescript";
import * as sessionContract from "./mcp-sessions.ts";
import { appActivityIssue } from "./app-activity-error.ts";

const require = createRequire(import.meta.url);

/** Executes the actual UI module with only its transport or hooks replaced by deterministic fixtures. */
function loadModule(relativePath, modules) {
  const source = readFileSync(new URL(relativePath, import.meta.url), "utf8");
  const { outputText } = ts.transpileModule(source, { compilerOptions: {
    target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX,
  } });
  const result = { exports: {} };
  // Production dependencies remain real unless a specific side-effect boundary is stubbed.
  const dependency = (name) => modules[name] ?? require(name);
  new Function("require", "module", "exports", outputText)(dependency, result, result.exports);
  return result.exports;
}

const views = loadModule("../components/mcp/McpSessionsPanel.tsx", {
  "~/lib/mcp-sessions": sessionContract,
  "./useMcpSessionsPage": {},
});

/** Uses documentation-only network addresses and synthetic identifiers, never connected-account data. */
function session(overrides = {}) {
  return {
    id: "session-row", session_id: "synthetic-session", protocol_version: "2025-03-26",
    started_at: "2026-08-26T10:00:00Z", last_activity_at: "2026-08-26T10:01:00Z",
    client_name: "Example MCP client", client_version: "1.2.3", initial_client_ip: "2001:db8::1",
    ...overrides,
  };
}

/** Supplies the public page view state without mounting any browser or transport effects. */
function history(overrides = {}) {
  return {
    page: { items: [session()], next_cursor: "next-page", has_more: true },
    pageNumber: 1, loading: false, error: "", next() {}, previous() {}, refresh() {}, retry() {},
    ...overrides,
  };
}

/** Renders the real JSX so disclosure and escaping assertions cover React's final markup. */
function render(name, props) {
  return renderToStaticMarkup(createElement(views[name], props));
}

/** Models React state/effect invalidation to exercise pagination and late network completion deterministically. */
function hookHarness() {
  const state = [];
  const effects = [];
  const requests = [];
  let stateIndex = 0;
  let effectIndex = 0;
  let dirty = true;
  let view;
  const react = {
    /** Retains each hook slot and batches state changes until the fixture renders again. */
    useState(initial) {
      const index = stateIndex++;
      // New mounts receive initial state exactly once, as React does.
      if (index === state.length) state.push(initial);
      return [state[index], (next) => {
        // Functional setters must observe the latest pending value during a batched transition.
        state[index] = typeof next === "function" ? next(state[index]) : next;
        dirty = true;
      }];
    },
    /** Runs cleanup before replacing an effect, preserving the stale-request fence under test. */
    useEffect(callback, dependencies) {
      const index = effectIndex++;
      const previous = effects[index];
      // Stable dependencies do not issue a second read on a state-only render.
      if (previous?.dependencies.every((value, position) => Object.is(value, dependencies[position]))) return;
      previous?.cleanup();
      effects[index] = { dependencies, cleanup: callback() };
    },
  };
  const { useMcpSessionsPage } = loadModule("../components/mcp/useMcpSessionsPage.ts", {
    react,
    "~/lib/mcp-sessions": sessionContract,
    "~/lib/app-activity-error": { appActivityIssue },
    "~/lib/api": { api: {
      /** Leaves each page request unresolved until the scenario supplies its response. */
      mcpGraphql(query, variables) {
        return new Promise((resolve, reject) => requests.push({ query, variables, resolve, reject }));
      },
    } },
  });
  /** Applies one batched interaction and its effects without manufacturing another fetch. */
  function flush() {
    // Effects can update state once; the bounded guard catches accidental render loops in the implementation.
    for (let remaining = 10; dirty && remaining > 0; remaining--) {
      dirty = false;
      stateIndex = 0;
      effectIndex = 0;
      view = useMcpSessionsPage("synthetic-app");
    }
    assert.equal(dirty, false, "session pagination must settle without a render loop");
    return view;
  }
  /** Lets the request's then/catch/finally chain complete before React applies the resulting state. */
  async function settle() {
    await new Promise((resolve) => setImmediate(resolve));
    return flush();
  }
  flush();
  return { requests, flush, settle, view: () => view };
}

// A loaded client is useful context but its name is never treated as verified identity.
test("session rows expose client/version and initially collapsed connection details on both layouts", () => {
  const html = render("McpSessionRows", { sessions: [session()] });
  assert.match(html, /Client \(self-reported\)/);
  assert.match(html, /Example MCP client/);
  assert.match(html, /Version: 1\.2\.3/);
  assert.match(html, /Initial client IP/);
  assert.match(html, /2001:db8::1/);
  assert.match(html, /proxy, VPN, or shared network/);
  assert.match(html, /<details class=/);
  assert.doesNotMatch(html, /<details[^>]*\bopen/);
  assert.match(html, /md:hidden/);
  assert.match(html, /hidden overflow-x-auto md:block/);
  assert.match(html, /break-all/);
  assert.match(html, /motion-safe:animate-pulse/);
});

// Older rows lack metadata and must not invent a client or a network address.
test("historical metadata remains Not recorded and client text is HTML escaped", () => {
  const missing = render("McpSessionRows", { sessions: [session({ client_name: "", client_version: undefined, initial_client_ip: "" })] });
  assert.match(missing, /Not recorded/);
  assert.doesNotMatch(missing, /Example MCP client|2001:db8/);
  const html = render("McpSessionRows", { sessions: [session({ client_name: '<img src=x onerror="alert(1)">', ended_at: "2026-08-26T11:00:00Z", end_reason: "client_closed" })] });
  assert.match(html, /&lt;img/);
  assert.doesNotMatch(html, /<img|>Live</);
  assert.match(html, /Disconnected/);
  assert.match(html, /client closed/);
});

// Loading, failed, and empty reads are distinct and never expose stale rows as the requested page.
test("session page states distinguish loading, failure, and empty history", () => {
  assert.match(render("McpSessionPageContent", { history: history({ loading: true }) }), /role="status"/);
  assert.doesNotMatch(render("McpSessionPageContent", { history: history({ loading: true }) }), /synthetic-session/);
  assert.match(render("McpSessionPageContent", { history: history({ error: "Unavailable" }) }), /role="alert"/);
  assert.match(render("McpSessionPageContent", { history: history({ error: "Unavailable" }) }), /Try again/);
  assert.match(render("McpSessionPageContent", { history: history({ page: { items: [], next_cursor: "", has_more: false } }) }), /No sessions recorded on this page/);
});

// The footer uses only actual server continuation evidence, not the page length or a guessed total.
test("pagination disables unavailable directions and remains responsive", () => {
  const first = render("McpSessionPagination", { history: history() });
  assert.match(first, /<button[^>]*disabled=""[^>]*>Previous/);
  assert.doesNotMatch(first, /<button[^>]*disabled=""[^>]*>Next/);
  assert.match(first, /flex-wrap/);
  const last = render("McpSessionPagination", { history: history({ pageNumber: 2, page: { items: [session()], next_cursor: "", has_more: false } }) });
  assert.doesNotMatch(last, /<button[^>]*disabled=""[^>]*>Previous/);
  assert.match(last, /<button[^>]*disabled=""[^>]*>Next/);
  const pending = render("McpSessionPagination", { history: history({ loading: true }) });
  assert.equal((pending.match(/disabled=""/g) || []).length, 2);
});

// Every navigation reads one exact-version page; browsing does not append session rows indefinitely.
test("next, previous, and refresh use server cursors and retain only one page", async () => {
  const check = hookHarness();
  assert.equal(check.requests.length, 1);
  assert.deepEqual(check.requests[0].variables, { appId: "synthetic-app", after: "", first: 25 });
  assert.match(check.requests[0].query, /mcpSessions\(app_id: \$appId, after: \$after, first: \$first\)/);
  check.view().next();
  check.flush();
  assert.equal(check.requests.length, 1, "pending reads cannot advance");
  check.requests[0].resolve({ mcpSessions: { items: [session()], next_cursor: "cursor-two", has_more: true } });
  await check.settle();
  check.view().next();
  assert.equal(check.flush().page, null);
  assert.equal(check.requests[1].variables.after, "cursor-two");
  check.requests[1].resolve({ mcpSessions: { items: [session({ id: "second", session_id: "only-page-two" })], next_cursor: "", has_more: false } });
  await check.settle();
  assert.deepEqual(check.view().page.items.map((item) => item.id), ["second"]);
  check.view().next();
  check.flush();
  assert.equal(check.requests.length, 2, "last page cannot advance without a cursor");
  check.view().previous();
  check.flush();
  assert.equal(check.requests[2].variables.after, "");
  check.requests[2].resolve({ mcpSessions: { items: [session()], next_cursor: "cursor-two", has_more: true } });
  await check.settle();
  check.view().next();
  check.flush();
  check.requests[3].resolve({ mcpSessions: { items: [session({ id: "second" })], next_cursor: "", has_more: false } });
  await check.settle();
  check.view().refresh();
  assert.equal(check.flush().pageNumber, 1);
  assert.equal(check.requests[4].variables.after, "");
});

// Abandoned network responses and sensitive error text must not replace the active view.
test("stale responses are discarded and failed cursor reads can be retried safely", async () => {
  const check = hookHarness();
  check.view().refresh();
  check.flush();
  check.requests[0].resolve({ mcpSessions: { items: [session()], next_cursor: "stale", has_more: true } });
  await check.settle();
  assert.equal(check.view().page, null);
  assert.equal(check.view().loading, true);
  check.requests[1].reject(new Error("private-network-sentinel"));
  await check.settle();
  assert.equal(check.view().loading, false);
  assert.match(check.view().error, /temporarily unavailable/);
  assert.doesNotMatch(check.view().error, /private-network-sentinel/);
  check.view().retry();
  check.flush();
  assert.deepEqual(check.requests[2].variables, check.requests[1].variables);
  check.requests[2].reject(new Error("app not found"));
  await check.settle();
  assert.match(check.view().error, /This MCP server is not active on this Engine/);
  assert.doesNotMatch(check.view().error, /app not found/);
});

// The route's existing exact app/audit gate must precede all session metadata reads.
test("the permission-gated sessions tab mounts cursor history, not a latest-ten preview", () => {
  const route = readFileSync(new URL("../routes/integrations.mcp_.$id.analytics.tsx", import.meta.url), "utf8");
  assert.match(route, /hasResourcePermission\(access, "app\.read", "APP", appFamilyId\) && hasWorkspacePermission\(access, "audit\.read"\)/);
  assert.match(route, /if \(!canReadRequests\) return/);
  assert.match(route, /<McpSessionsPanel key=\{id\} appId=\{id\}/);
  assert.doesNotMatch(route, /recent_sessions/);
  assert.match(sessionContract.MCP_SESSIONS_QUERY, /client_name client_version initial_client_ip/);
});
