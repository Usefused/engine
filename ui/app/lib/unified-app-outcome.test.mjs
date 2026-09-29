import assert from "node:assert/strict";
import test from "node:test";
import { unifiedAppFailureStage, unifiedAppOutcome } from "./unified-app-outcome.ts";

// Ordinary readers receive useful phase descriptions without raw exception or payload material.
test("safe outcomes describe the recorded phase without exposing private text", () => {
  const event = { status: "failed", failure_code: "execution_failed", failure_reason: "unified_app_execute_failed", error: "private exception", request: "private request", response: "private response" };
  assert.equal(unifiedAppFailureStage(event), "execute");
  assert.match(unifiedAppOutcome(event).message, /running its code/);
  assert.doesNotMatch(JSON.stringify(unifiedAppOutcome(event)), /private/);
});

// Historical and malformed fields cannot be rendered as raw error prose or converted into invented stages.
test("unknown failures and partial timings remain unclassified", () => {
  const event = { status: "failed", failure_code: "private error", failure_reason: "private payload", timings: [{ name: "unified_app_execute", duration_ms: 12 }] };
  assert.equal(unifiedAppFailureStage(event), undefined);
  assert.equal(unifiedAppOutcome(event).message, "Failure stage not recorded.");
  assert.doesNotMatch(JSON.stringify(unifiedAppOutcome(event)), /private/);
});

// Known schema failures are informative even when older receipts have no phase metadata.
test("validation codes identify schema failures without disclosing field values", () => {
  assert.equal(unifiedAppFailureStage({ status: "failed", failure_code: "input_validation_failed" }), "input_validation");
  assert.equal(unifiedAppFailureStage({ status: "failed", failure_code: "output_validation_failed" }), "output_validation");
});

// Interrupted effects retain uncertainty while a successful root is not mislabeled by stale failure metadata.
test("interruption and success keep their authoritative outcome", () => {
  const event = { status: "failed", failure_code: "execution_interrupted", failure_reason: "unified_app_execute_failed" };
  assert.match(unifiedAppOutcome(event).message, /unknown/);
  assert.match(unifiedAppOutcome(event).action, /before retrying/);
  assert.equal(unifiedAppFailureStage({ ...event, status: "success" }), undefined);
  assert.equal(unifiedAppOutcome({ ...event, status: "success" }).title, "Execution completed");
  assert.equal(unifiedAppOutcome({ status: "failed", failure_code: "execution_timeout" }).title, "Execution timed out");
});
