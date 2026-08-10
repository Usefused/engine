import assert from "node:assert/strict";
import test from "node:test";

import {
  RATE_LIMIT_GRAPHQL_FIELDS,
  createRateLimitConfig,
  policyWithAlgorithm,
  rateLimitSummary,
} from "./rate-limit.ts";

test("projects the complete frozen v2 GraphQL contract without legacy fields", () => {
  for (const field of [
    "version",
    "policies",
    "default_cost",
    "operation_costs",
    "fixed_window",
    "duration_ms",
    "token_bucket",
    "refill_units",
    "refill_interval_ms",
    "response_headers",
    "retry_after",
    "max_delay_ms",
  ]) {
    assert.match(RATE_LIMIT_GRAPHQL_FIELDS, new RegExp(`\\b${field}\\b`));
  }
  assert.doesNotMatch(
    RATE_LIMIT_GRAPHQL_FIELDS,
    /requests_per_second|requests_per_minute|\bburst\b|\bstrategy\b/
  );
});

test("creates exact v2 policies and switches discriminator branches atomically", () => {
  const config = createRateLimitConfig();
  assert.equal(config.version, 2);
  assert.equal(config.policies[0].algorithm, "fixed_window");
  assert.ok(config.policies[0].fixed_window);
  assert.equal(config.policies[0].token_bucket, undefined);

  const bucket = policyWithAlgorithm(config.policies[0], "token_bucket");
  assert.equal(bucket.fixed_window, undefined);
  assert.deepEqual(bucket.token_bucket, {
    capacity: 100,
    refill_units: 10,
    refill_interval_ms: 1000,
  });
  assert.equal("burst" in bucket.token_bucket, false);
});

test("summarizes absent, singular, and multiple policies", () => {
  assert.equal(rateLimitSummary(null), "Not declared");
  const config = createRateLimitConfig();
  assert.equal(rateLimitSummary(config), "1 policy");
  config.policies.push({ ...config.policies[0], name: "burst" });
  assert.equal(rateLimitSummary(config), "2 policies");
});
