export type RateLimitUnit = "requests" | "points" | "quota_units";
export type RateLimitScope = "service_version" | "connection";
export type RateLimitAlgorithm = "fixed_window" | "token_bucket";
export type RateLimitResetFormat =
  | "delta_seconds"
  | "unix_seconds"
  | "unix_milliseconds"
  | "rfc3339";

export interface RateLimitConfig {
  version: 2;
  policies: RateLimitPolicy[];
  retry_after?: RateLimitRetryAfter;
}

export interface RateLimitPolicy {
  name: string;
  unit: RateLimitUnit;
  scope: RateLimitScope;
  default_cost: number;
  operation_costs: Record<string, number>;
  algorithm: RateLimitAlgorithm;
  fixed_window?: RateLimitFixedWindow;
  token_bucket?: RateLimitTokenBucket;
  response_headers?: RateLimitResponseHeaders;
}

export interface RateLimitFixedWindow {
  limit: number;
  duration_ms: number;
}

export interface RateLimitTokenBucket {
  capacity: number;
  refill_units: number;
  refill_interval_ms: number;
}

export interface RateLimitResponseHeaders {
  limit?: string;
  remaining?: string;
  reset?: {
    name: string;
    format: RateLimitResetFormat;
  };
}

export interface RateLimitRetryAfter {
  enabled: boolean;
  max_delay_ms: number;
}

// Every UI query uses this single projection so an exact wire-contract change
// cannot leave a loader and its client refresh path decoding different shapes.
export const RATE_LIMIT_GRAPHQL_FIELDS = `
  version
  policies {
    name
    unit
    scope
    default_cost
    operation_costs
    algorithm
    fixed_window { limit duration_ms }
    token_bucket { capacity refill_units refill_interval_ms }
    response_headers { limit remaining reset { name format } }
  }
  retry_after { enabled max_delay_ms }
`;

export function createRateLimitConfig(): RateLimitConfig {
  return { version: 2, policies: [createRateLimitPolicy()] };
}

export function createRateLimitPolicy(
  algorithm: RateLimitAlgorithm = "fixed_window"
): RateLimitPolicy {
  const policy: RateLimitPolicy = {
    name: "requests",
    unit: "requests",
    scope: "service_version",
    default_cost: 1,
    operation_costs: {},
    algorithm,
  };
  return policyWithAlgorithm(policy, algorithm);
}

export function policyWithAlgorithm(
  policy: RateLimitPolicy,
  algorithm: RateLimitAlgorithm
): RateLimitPolicy {
  // Switching the discriminator must switch its branch atomically; retaining
  // both would create a document Engine correctly rejects as ambiguous.
  if (algorithm === "fixed_window") {
    return {
      ...policy,
      algorithm,
      fixed_window: policy.fixed_window ?? { limit: 1000, duration_ms: 60_000 },
      token_bucket: undefined,
    };
  }
  return {
    ...policy,
    algorithm,
    fixed_window: undefined,
    token_bucket: policy.token_bucket ?? {
      capacity: 100,
      refill_units: 10,
      refill_interval_ms: 1000,
    },
  };
}

export function replaceRateLimitPolicy(
  config: RateLimitConfig,
  index: number,
  policy: RateLimitPolicy
): RateLimitConfig {
  const policies = config.policies.map((current, currentIndex) =>
    currentIndex === index ? policy : current
  );
  return { ...config, policies };
}

export function removeRateLimitPolicy(
  config: RateLimitConfig,
  index: number
): RateLimitConfig {
  return {
    ...config,
    policies: config.policies.filter((_, currentIndex) => currentIndex !== index),
  };
}

export function rateLimitSummary(config?: RateLimitConfig | null): string {
  if (!config) return "Not declared";
  const count = config.policies.length;
  return `${count} ${count === 1 ? "policy" : "policies"}`;
}
