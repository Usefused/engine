import assert from "node:assert/strict";
import test from "node:test";
import {
  closeDiscoverySessionQuery,
  discoveryNavigationFromQuery,
  openDiscoverySessionQuery,
  validOpaqueDiscoverySession,
} from "./discovery-navigation.ts";

test("admits the exact CLI discovery review handoff without assuming UUID identity", () => {
  const navigation = discoveryNavigationFromQuery(new URLSearchParams("handoff=cli&session=run%3A%E8%AE%A1%E5%88%92%2Bopaque~1&tab=pending"));
  assert.deepEqual(navigation, { sessionID: "run:计划+opaque~1", cliHandoff: true });
});

test("rejects ambiguous, unsafe, and oversized discovery navigation", () => {
  const invalidQueries = [
    "session=one&session=two&handoff=cli",
    "session=one&handoff=cli&handoff=ui",
    "session=one&handoff=unknown",
    "session=private%20child&handoff=cli",
    "session=%2Fprivate&handoff=cli",
    "session=private%5Cchild&handoff=cli",
    "session=private%09child&handoff=cli",
    "session=private%0Dchild&handoff=cli",
    "session=private%0Achild&handoff=cli",
    "session=private%00child&handoff=cli",
    "session=%C2%A0private&handoff=cli",
    `session=${"计".repeat(43)}&handoff=cli`,
  ];
  for (const query of invalidQueries) {
    assert.deepEqual(discoveryNavigationFromQuery(new URLSearchParams(query)), { sessionID: null, cliHandoff: false });
  }
  assert.equal(validOpaqueDiscoverySession("a".repeat(128)), true);
  assert.equal(validOpaqueDiscoverySession("ordinary:ui+session"), true);
});

test("UI resumes remove CLI handoff while close clears the complete discovery navigation", () => {
  const cliQuery = new URLSearchParams("handoff=cli&session=cli-run&tab=pending&q=github");
  const uiQuery = openDiscoverySessionQuery(cliQuery, "ui-run");
  assert.equal(uiQuery.get("session"), "ui-run");
  assert.equal(uiQuery.has("handoff"), false);
  assert.equal(uiQuery.get("tab"), "pending");
  assert.equal(uiQuery.get("q"), "github");

  const closed = closeDiscoverySessionQuery(cliQuery);
  assert.equal(closed.has("session"), false);
  assert.equal(closed.has("handoff"), false);
  assert.equal(closed.get("tab"), "pending");
});
