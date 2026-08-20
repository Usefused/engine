import assert from "node:assert/strict";
import test from "node:test";

import { RETRY_GRAPHQL_FIELDS, retrySummary } from "./retry-policy.ts";

// This test locks the UI query to the Registry's complete v3 retry shape.
test("projects the complete v3 GraphQL contract without removed legacy fields", () => {
  for (const field of [
    "version",
    "rules",
    "predicates",
    "methods",
    "operation_kinds",
    "statuses",
    "errors",
    "body_replayability",
    "idempotency_key",
    "required_provider_headers",
    "action",
    "max_attempts",
    "max_elapsed_ms",
    "backoff",
    "retry_after_headers",
  ]) {
    assert.match(RETRY_GRAPHQL_FIELDS, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(
    RETRY_GRAPHQL_FIELDS,
    /\bmax_retries\b|\bbackoff_ms\b/
  );
});

// This test keeps the summary stable for absent, singular, and plural rules.
test("summarizes absent, singular, and multiple retry rules", () => {
  assert.equal(retrySummary(null), "Not declared");
  const rule = {
    predicates: {
      methods: ["GET"],
      operation_kinds: [],
      statuses: [{ min: 500, max: 599 }],
      errors: [],
      body_replayability: "any",
      idempotency_key: { requirement: "any" },
      required_provider_headers: [],
    },
    action: {
      max_attempts: 2,
      max_elapsed_ms: 1000,
      backoff: {
        strategy: "fixed",
        base_delay_ms: 100,
        max_delay_ms: 100,
        jitter_ms: 0,
      },
      retry_after_headers: [],
    },
  };
  assert.equal(retrySummary({ version: 3, rules: [rule] }), "1 rule");
  assert.equal(retrySummary({ version: 3, rules: [rule, rule] }), "2 rules");
});
