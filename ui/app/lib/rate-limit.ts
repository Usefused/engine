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

// rateLimitDurationLabel turns exact millisecond windows into compact labels
// that remain legible inside the narrow service-configuration cards.
export function rateLimitDurationLabel(durationMs: number): string {
  // Prefer the largest exact unit so the label never rounds a configured limit.
  if (durationMs > 0 && durationMs % 3_600_000 === 0) {
    return `${durationMs / 3_600_000} hr`;
  }
  if (durationMs > 0 && durationMs % 60_000 === 0) {
    return `${durationMs / 60_000} min`;
  }
  if (durationMs > 0 && durationMs % 1_000 === 0) {
    return `${durationMs / 1_000} sec`;
  }
  return `${durationMs} ms`;
}

// rateLimitAlgorithmLabel presents contract identifiers as readable UI copy.
export function rateLimitAlgorithmLabel(algorithm: RateLimitAlgorithm): string {
  const labels: Record<RateLimitAlgorithm, string> = {
    fixed_window: "Fixed window",
    rolling_window: "Rolling window",
    token_bucket: "Token bucket",
    concurrency: "Concurrency",
  };
  return labels[algorithm];
}

// rateLimitPolicyName preserves the configured words while removing identifier
// punctuation that creates awkward wrapping in compact cards.
export function rateLimitPolicyName(name: string): string {
  const readable = name.replace(/[_-]+/g, " ").trim();
  // Empty names are invalid upstream, but retaining a stable fallback keeps the
  // details view usable if it encounters an older malformed snapshot.
  if (!readable) return "Policy";
  return readable.charAt(0).toUpperCase() + readable.slice(1);
}

// rateLimitPolicyQuotaLabel makes the effective quota the primary policy detail.
export function rateLimitPolicyQuotaLabel(policy: RateLimitPolicy): string {
  const window = policy.fixed_window ?? policy.rolling_window;
  if (window) {
    return `${window.limit} ${policy.unit} / ${rateLimitDurationLabel(window.duration_ms)}`;
  }
  if (policy.token_bucket) {
    const refill = rateLimitDurationLabel(policy.token_bucket.refill_interval_ms);
    return `${policy.token_bucket.capacity} ${policy.unit} · +${policy.token_bucket.refill_units} / ${refill}`;
  }
  if (policy.concurrency) {
    return `${policy.concurrency.limit} concurrent ${policy.unit}`;
  }
  // The algorithm-specific object is required by the contract; this fallback
  // avoids inventing a quota when rendering a partially migrated snapshot.
  return `${policy.unit} policy`;
}
