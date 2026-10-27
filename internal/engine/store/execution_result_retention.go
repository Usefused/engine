package store

// PostgresStoreOption configures storage before any execution can be admitted.
type PostgresStoreOption func(*postgresStore)

// WithExecutionResultRetentionDays lets database operators retain results indefinitely with zero.
func WithExecutionResultRetentionDays(days int) PostgresStoreOption {
	// Capture configuration once rather than consulting mutable process state during completion.
	return func(s *postgresStore) { s.executionResultRetentionDays = &days }
}

// resultRetentionDays keeps the hosted default at one week without imposing a maximum on operators.
func (s *postgresStore) resultRetentionDays() int {
	// Unconfigured stores and invalid internal callers receive the same safe default.
	if s.executionResultRetentionDays == nil || *s.executionResultRetentionDays < 0 {
		return 7
	}
	return *s.executionResultRetentionDays
}
