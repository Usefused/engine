import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import { URL } from "node:url";

import {
  RATE_LIMIT_GRAPHQL_FIELDS,
  rateLimitAlgorithmLabel,
  rateLimitDurationLabel,
  rateLimitPolicyName,
  rateLimitPolicyQuotaLabel,
  rateLimitSummary,
} from "./rate-limit.ts";

// This test locks the UI query to the Registry's complete v3 rate-limit shape.
test("projects the complete v3 GraphQL contract without removed v2 fields", () => {
  for (const field of [
    "version",
    "policies",
    "mode",
    "identity",
    "cost",
    "fixed_window",
    "rolling_window",
    "token_bucket",
    "concurrency",
    "response_signals",
    "cooldown",
    "max_delay_ms",
  ]) {
    assert.match(RATE_LIMIT_GRAPHQL_FIELDS, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(
    RATE_LIMIT_GRAPHQL_FIELDS,
    /\bscope\b|default_cost|operation_costs|response_headers|retry_after/
  );
});

// This test keeps the summary stable for absent, singular, and plural policies.
test("summarizes absent, singular, and multiple policies", () => {
  assert.equal(rateLimitSummary(null), "Not declared");
  const policy = {
    name: "requests",
    mode: "enforce",
    unit: "requests",
    identity: { inputs: [{ kind: "service_version" }] },
    cost: { default: 1, rules: [] },
    algorithm: "fixed_window",
    fixed_window: { limit: 10, duration_ms: 1000 },
  };
  assert.equal(rateLimitSummary({ version: 3, policies: [policy] }), "1 policy");
  assert.equal(
    rateLimitSummary({ version: 3, policies: [policy, { ...policy, name: "burst" }] }),
    "2 policies"
  );
});

// This test keeps compact policy copy readable without rounding exact windows.
test("formats policy names, algorithms, durations, and effective quotas", () => {
  assert.equal(rateLimitPolicyName("second_requests"), "Second requests");
  assert.equal(rateLimitPolicyName(""), "Policy");
  assert.equal(rateLimitAlgorithmLabel("fixed_window"), "Fixed window");
  assert.equal(rateLimitDurationLabel(1000), "1 sec");
  assert.equal(rateLimitDurationLabel(60_000), "1 min");
  assert.equal(rateLimitDurationLabel(3_600_000), "1 hr");
  assert.equal(rateLimitDurationLabel(1500), "1500 ms");

  const fixedWindow = {
    name: "second_requests",
    mode: "enforce",
    unit: "requests",
    identity: { inputs: [{ kind: "connection" }] },
    cost: { default: 1, rules: [] },
    algorithm: "fixed_window",
    fixed_window: { limit: 100, duration_ms: 1000 },
  };
  assert.equal(rateLimitPolicyQuotaLabel(fixedWindow), "100 requests / 1 sec");
  assert.equal(
    rateLimitPolicyQuotaLabel({
      ...fixedWindow,
      algorithm: "token_bucket",
      fixed_window: undefined,
      token_bucket: { capacity: 20, refill_units: 5, refill_interval_ms: 60_000 },
    }),
    "20 requests · +5 / 1 min"
  );
  assert.equal(
    rateLimitPolicyQuotaLabel({
      ...fixedWindow,
      algorithm: "concurrency",
      fixed_window: undefined,
      concurrency: { limit: 4 },
    }),
    "4 concurrent requests"
  );
});

// This test protects the actual service route that produced the hosted schema error.
test("service details uses only the shared v3 rate and retry projections", () => {
  const source = readFileSync(
    new URL("../routes/integrations.$id.tsx", import.meta.url),
    "utf8"
  );
  assert.match(source, /rate_limit \{ \$\{RATE_LIMIT_GRAPHQL_FIELDS\} \}/);
  assert.match(source, /retry_config \{ \$\{RETRY_GRAPHQL_FIELDS\} \}/);
  assert.doesNotMatch(
    source,
    /policy\.scope|policy\.default_cost|retry\.strategy|retry\.max_retries|retry\.backoff_ms/
  );
});
