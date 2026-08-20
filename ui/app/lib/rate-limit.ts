export type RateLimitUnit =
  | "requests"
  | "points"
  | "complexity"
  | "quota_units";
export type RateLimitMode = "enforce" | "observe";
export type RateLimitAlgorithm =
  | "fixed_window"
  | "rolling_window"
  | "token_bucket"
  | "concurrency";

export interface RateLimitConfig {
  version: 3;
  policies: RateLimitPolicy[];
  cooldown?: RateLimitCooldown;
}

export interface RateLimitPolicy {
  name: string;
  mode: RateLimitMode;
  unit: RateLimitUnit;
  identity: RateLimitBucketIdentity;
  cost: RateLimitCostPlan;
  algorithm: RateLimitAlgorithm;
  fixed_window?: RateLimitWindow;
  rolling_window?: RateLimitWindow;
  token_bucket?: RateLimitTokenBucket;
  concurrency?: RateLimitConcurrency;
  response_signals?: RateLimitResponseSignals;
}

export interface RateLimitBucketIdentity {
  inputs: RateLimitIdentityInput[];
}

export interface RateLimitIdentityInput {
  kind: string;
  binding?: string;
  name?: string;
}

export interface RateLimitCostPlan {
  default: number;
  rules: Array<{ operation: string; cost: number }>;
}

export interface RateLimitWindow {
  limit: number;
  duration_ms: number;
}

export interface RateLimitTokenBucket {
  capacity: number;
  refill_units: number;
  refill_interval_ms: number;
}

export interface RateLimitConcurrency {
  limit: number;
}

export interface RateLimitSignal {
  source: string;
  name?: string;
  path?: string;
}

export interface RateLimitResponseSignals {
  limit?: RateLimitSignal;
  remaining?: RateLimitSignal;
  reset?: { signal: RateLimitSignal; format: string };
  cost?: RateLimitSignal;
}

export interface RateLimitCooldown {
  statuses: Array<{ min: number; max: number }>;
  headers: Array<{ name: string; formats: string[]; max_delay_ms: number }>;
}

// Every service-detail query uses the Registry's exact v3 projection so loader
// and client refreshes cannot drift onto removed v2 fields independently.
export const RATE_LIMIT_GRAPHQL_FIELDS = `
  version
  policies {
    name
    mode
    unit
    identity { inputs { kind binding name } }
    cost { default rules { operation cost } }
    algorithm
    fixed_window { limit duration_ms }
    rolling_window { limit duration_ms }
    token_bucket { capacity refill_units refill_interval_ms }
    concurrency { limit }
    response_signals {
      limit { source name path }
      remaining { source name path }
      reset { signal { source name path } format }
      cost { source name path }
    }
  }
  cooldown {
    statuses { min max }
    headers { name formats max_delay_ms }
  }
`;

// rateLimitSummary provides a compact label without hiding the full policies.
export function rateLimitSummary(config?: RateLimitConfig | null): string {
  if (!config) return "Not declared";
  const count = config.policies.length;
  return `${count} ${count === 1 ? "policy" : "policies"}`;
}
