package ratelimitpolicy

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// ResolvedPolicy is the bounded, execution-ready form passed to the shared
// quota coordinator.
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
	Allowed    bool
	RetryAfter time.Duration
	// CoordinationAttempts is safe, bounded contention telemetry. It is not
	// part of the persisted Activity contract.
	CoordinationAttempts int64
	LocalLease           bool
	PolicyCount          int64
	ScopeKinds           []string
	UnitTotals           map[string]int64
	HeaderOutcome        string
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

const ProviderRateLimitStateSchemaVersion = 1

// StateEnvelope is the complete, atomically replaced JetStream value for one
// execution quota scope. Keeping every AND policy in one document avoids a
// partial consumption when one of several provider windows is exhausted.
type StateEnvelope struct {
	SchemaVersion    int           `json:"schema_version"`
	AccountID        uuid.UUID     `json:"account_id"`
	ServiceVersionID uuid.UUID     `json:"service_version_id"`
	Policies         []PolicyState `json:"policies"`
	CooldownUntil    *time.Time    `json:"cooldown_until,omitempty"`
	ControlEpoch     uint64        `json:"control_epoch"`
	Sequence         uint64        `json:"sequence"`
	UpdatedAt        time.Time     `json:"updated_at"`
}

// PolicyState is safe coordination state. Provider credentials, response
// header values, and resolved URLs must never enter JetStream or PostgreSQL.
type PolicyState struct {
	Name                 string     `json:"name"`
	ScopeKind            string     `json:"scope_kind"`
	ScopeID              uuid.UUID  `json:"scope_id"`
	ConfigHash           string     `json:"config_hash"`
	Algorithm            string     `json:"algorithm"`
	FixedWindowStartedAt *time.Time `json:"fixed_window_started_at,omitempty"`
	FixedWindowUsed      int64      `json:"fixed_window_used"`
	Tokens               *int64     `json:"tokens,omitempty"`
	TokenRefilledAt      *time.Time `json:"token_refilled_at,omitempty"`
}

func (s StateEnvelope) Validate() error {
	if s.SchemaVersion != ProviderRateLimitStateSchemaVersion || s.AccountID == uuid.Nil || s.ServiceVersionID == uuid.Nil || s.UpdatedAt.IsZero() {
		return errors.New("provider rate-limit state envelope is invalid")
	}
	if len(s.Policies) == 0 || len(s.Policies) > MaxPolicies {
		return errors.New("provider rate-limit state policy count is invalid")
	}
	for _, policy := range s.Policies {
		if err := policy.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (s PolicyState) validate() error {
	if err := s.validateIdentity(); err != nil {
		return err
	}
	switch s.Algorithm {
	case "fixed_window":
		if s.FixedWindowStartedAt != nil && s.Tokens == nil && s.TokenRefilledAt == nil {
			return nil
		}
	case "token_bucket":
		if validTokenState(s) {
			return nil
		}
	}
	return errors.New("provider rate-limit policy algorithm state is invalid")
}

func (s PolicyState) validateIdentity() error {
	if s.Name == "" || s.ScopeID == uuid.Nil || s.ConfigHash == "" {
		return errors.New("provider rate-limit policy state is invalid")
	}
	if s.ScopeKind != "service_version" && s.ScopeKind != "connection" {
		return errors.New("provider rate-limit policy scope is invalid")
	}
	if s.FixedWindowUsed < 0 || s.FixedWindowUsed > maxPolicyValue {
		return errors.New("provider rate-limit policy usage is invalid")
	}
	return nil
}

func validTokenState(state PolicyState) bool {
	return state.FixedWindowStartedAt == nil && state.FixedWindowUsed == 0 &&
		state.Tokens != nil && *state.Tokens >= 0 && *state.Tokens <= maxPolicyValue &&
		state.TokenRefilledAt != nil
}
