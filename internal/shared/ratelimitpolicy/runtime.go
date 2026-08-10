package ratelimitpolicy

import (
	"time"

	"github.com/google/uuid"
)

// ResolvedPolicy is the bounded, execution-ready form passed to PostgreSQL.
// It contains no credentials or provider response values.
type ResolvedPolicy struct {
	Name             string `json:"name"`
	Unit             string `json:"unit"`
	ScopeKind        string `json:"scope_kind"`
	ScopeID          string `json:"scope_id"`
	Cost             int64  `json:"cost"`
	Algorithm        string `json:"algorithm"`
	ConfigHash       string `json:"config_hash"`
	Limit            int64  `json:"limit"`
	DurationMS       int64  `json:"duration_ms"`
	Capacity         int64  `json:"capacity"`
	RefillUnits      int64  `json:"refill_units"`
	RefillIntervalMS int64  `json:"refill_interval_ms"`
}

type AcquireRequest struct {
	AccountID        uuid.UUID
	ServiceVersionID uuid.UUID
	Policies         []ResolvedPolicy
}

type Decision struct {
	Allowed       bool
	RetryAfter    time.Duration
	PolicyCount   int64
	ScopeKinds    []string
	UnitTotals    map[string]int64
	HeaderOutcome string
}

// ResponseObservation contains only parsed numeric/timestamp bounds. Header
// names and values never cross into persistence or observability records.
type ResponseObservation struct {
	PolicyName string     `json:"policy_name"`
	ScopeKind  string     `json:"scope_kind"`
	ScopeID    uuid.UUID  `json:"scope_id"`
	Algorithm  string     `json:"algorithm"`
	LocalLimit int64      `json:"local_limit"`
	DurationMS int64      `json:"duration_ms"`
	Limit      *int64     `json:"limit,omitempty"`
	Remaining  *int64     `json:"remaining,omitempty"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
}

type SyncRequest struct {
	AccountID        uuid.UUID
	ServiceVersionID uuid.UUID
	CooldownUntil    *time.Time
	Observations     []ResponseObservation
}
