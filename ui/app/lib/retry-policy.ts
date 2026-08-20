export interface RetryConfig {
  version: 3;
  rules: RetryRule[];
}

export interface RetryRule {
  predicates: RetryPredicates;
  action: RetryAction;
}

export interface RetryPredicates {
  methods: string[];
  operation_kinds: string[];
  statuses: Array<{ min: number; max: number }>;
  errors: string[];
  body_replayability: string;
  idempotency_key: { requirement: string; header?: string };
  required_provider_headers: string[];
}

export interface RetryAction {
  max_attempts: number;
  max_elapsed_ms: number;
  backoff: {
    strategy: string;
    base_delay_ms: number;
    max_delay_ms: number;
    jitter_ms: number;
  };
  retry_after_headers: Array<{
    name: string;
    formats: string[];
    max_delay_ms: number;
  }>;
}

// The shared projection mirrors the Registry's v3 RetryConfig and prevents
// route-specific queries from retaining removed legacy retry fields.
export const RETRY_GRAPHQL_FIELDS = `
  version
  rules {
    predicates {
      methods
      operation_kinds
      statuses { min max }
      errors
      body_replayability
      idempotency_key { requirement header }
      required_provider_headers
    }
    action {
      max_attempts
      max_elapsed_ms
      backoff { strategy base_delay_ms max_delay_ms jitter_ms }
      retry_after_headers { name formats max_delay_ms }
    }
  }
`;

// retrySummary describes the number of independently matched v3 retry rules.
export function retrySummary(config?: RetryConfig | null): string {
  if (!config) return "Not declared";
  const count = config.rules.length;
  return `${count} ${count === 1 ? "rule" : "rules"}`;
}
