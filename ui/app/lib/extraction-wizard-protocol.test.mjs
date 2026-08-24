import assert from "node:assert/strict";
import test from "node:test";

import {
  cancelDiscoveryAction,
  discoveryReviewReceiptsMatch,
  enrichmentDecisionAction,
  initialOperationSelectionIDs,
  operationSelectionID,
  parseDiscoveryEnvelope,
  parseOverlayObject,
  preferNewerSnapshot,
  requestPlanAction,
  reviewableDiscoveryReceipt,
  reviewSummaryMatchesSnapshot,
  selectOperationsAction,
  updateOverlayAction,
  visibleDiscoveryError,
} from "./extraction-wizard-protocol.ts";

const operations = [
  { method: "GET", path: "/users", summary: "List users", occurrences: 2 },
  { method: "POST", path: "/users", summary: "Create user", occurrences: 1 },
];

const reviewSnapshot = {
  version: 1,
  session_id: "session-1",
  revision: 7,
  draft_revision: 3,
  state: "awaiting_review",
  payload: { effective_workers: 2, max_pages: 20, max_depth: 2, max_selections: 10 },
};

test("uses exact method/path identities for the server-bounded initial selection", () => {
  assert.equal(operationSelectionID(operations[0]), "GET|/users");
  assert.deepEqual([...initialOperationSelectionIDs(operations, 1)], ["GET|/users"]);
  assert.deepEqual([...initialOperationSelectionIDs(operations, 0)], []);
});

test("accepts only current version-one envelopes for the active session", () => {
  const valid = JSON.stringify({
    version: 1,
    session_id: "session-1",
    revision: 8,
    state: "awaiting_review",
    type: "review_required",
  });
  assert.equal(parseDiscoveryEnvelope(valid, "session-1", 7)?.revision, 8);
  assert.equal(parseDiscoveryEnvelope(valid, "other-session", 7), null);
  assert.equal(parseDiscoveryEnvelope(valid, "session-1", 9), null);
  assert.equal(parseDiscoveryEnvelope(valid.replace('"version":1', '"version":2'), "session-1", 7), null);
  assert.equal(parseDiscoveryEnvelope(valid.replace('"state":"awaiting_review"', '"state":"crawl_docs"'), "session-1", 7), null);
  assert.equal(parseDiscoveryEnvelope("not-json", "session-1", 7), null);
  assert.equal(parseDiscoveryEnvelope(valid.replace('"type":"review_required"', '"type":"completed"'), "session-1", 7), null);
  assert.equal(parseDiscoveryEnvelope(valid.replace('"state":"awaiting_review"', '"state":"done"'), "session-1", 7), null);
});

test("binds review summaries to the exact public snapshot receipt", () => {
  const contract = { draft_id: "draft-1", draft_revision: 3, review_hash: "a".repeat(64) };
  const snapshot = { ...reviewSnapshot, payload: { ...reviewSnapshot.payload, contract } };
  const summary = {
    schema_version: 1,
    session_id: snapshot.session_id,
    ...contract,
    info: { title: "Users", version: "1" },
    server_counts: { total: 0, returned: 0, omitted: 0 },
    auth_scheme_counts: { total: 0, returned: 0, omitted: 0 },
    operation_counts: { total: 0, returned: 0, omitted: 0 },
    diagnostic_count: 0,
    evidence_count: 1,
  };
  assert.deepEqual(reviewableDiscoveryReceipt(snapshot), contract);
  assert.equal(reviewSummaryMatchesSnapshot(summary, snapshot), true);
  assert.equal(reviewSummaryMatchesSnapshot({ ...summary, review_hash: "b".repeat(64) }, snapshot), false);
  assert.equal(discoveryReviewReceiptsMatch(contract, { ...contract }), true);
  assert.equal(reviewableDiscoveryReceipt({ ...snapshot, state: "extract_contract" }), null);
});

test("keeps a newer authoritative snapshot when GET responses settle out of order", () => {
  const older = { ...reviewSnapshot, revision: 6 };
  assert.equal(preferNewerSnapshot(reviewSnapshot, older), reviewSnapshot);
  assert.equal(preferNewerSnapshot(null, reviewSnapshot), reviewSnapshot);
});

test("builds exact revision-bound operation and draft actions", () => {
  const selection = selectOperationsAction(reviewSnapshot, operations.map(({ method, path }) => ({ method, path })));
  assert.deepEqual(selection, {
    version: 1,
    session_id: "session-1",
    expected_revision: 7,
    action: "select_operations",
    payload: { operations: [{ method: "GET", path: "/users" }, { method: "POST", path: "/users" }] },
  });
  assert.equal(enrichmentDecisionAction(reviewSnapshot, "accept_enrichment", ["proposal-1"]).draft_revision, 3);
  assert.equal(updateOverlayAction(reviewSnapshot, { "x-fused-connect": {} }).action, "update_overlay");
  assert.deepEqual(requestPlanAction(reviewSnapshot), {
    version: 1,
    session_id: "session-1",
    expected_revision: 7,
    draft_revision: 3,
    action: "request_plan",
  });
  assert.equal(cancelDiscoveryAction(reviewSnapshot).draft_revision, 3);
});

test("fails closed when a draft-bound action has no draft revision", () => {
  assert.throws(() => requestPlanAction({ ...reviewSnapshot, draft_revision: undefined }), /no reviewable draft revision/);
});

test("accepts only a structured object as a Fused overlay", () => {
  assert.deepEqual(parseOverlayObject('{"x-fused-connect":{}}'), { "x-fused-connect": {} });
  assert.throws(() => parseOverlayObject("[]"), /must be a JSON object/);
  assert.throws(() => parseOverlayObject("null"), /must be a JSON object/);
});

test("keeps a durable discovery error visible across transport recovery", () => {
  assert.equal(visibleDiscoveryError("Extraction failed", "Reconnecting"), "Extraction failed");
  assert.equal(visibleDiscoveryError("", "Reconnecting"), "Reconnecting");
});
