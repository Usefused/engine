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
	Name              string `json:"name"`
	Unit              string `json:"unit"`
	ScopeKind         string `json:"scope_kind"`
	ScopeID           string `json:"scope_id"`
	Mode              string `json:"mode"`
	Cost              int64  `json:"cost"`
	Algorithm         string `json:"algorithm"`
	ConfigHash        string `json:"config_hash"`
	Limit             int64  `json:"limit"`
	DurationMs        int64  `json:"duration_ms"`
	Capacity          int64  `json:"capacity"`
	RefillUnits       int64  `json:"refill_units"`
	RefillIntervalMs  int64  `json:"refill_interval_ms"`
	RollingLimit      int64  `json:"rolling_limit"`
	RollingDurationMs int64  `json:"rolling_duration_ms"`
	ConcurrencyLimit  int64  `json:"concurrency_limit"`
}

type AcquireRequest struct {
	AccountID        uuid.UUID
	ServiceVersionID uuid.UUID
	PermitID         uuid.UUID
	PermitExpiresAt  time.Time
	Policies         []ResolvedPolicy
}

// ReleaseRequest carries the exact identities acquired for concurrency
// policies. Reusing the resolved snapshot prevents a later config refresh from
// releasing a different bucket.
type ReleaseRequest = AcquireRequest

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
	ObservedDenials      int64
	ConcurrencyDenied    bool
}

// ResponseObservation contains only parsed numeric/timestamp bounds. Header
// names and values never cross into persistence or observability records.
type ResponseObservation struct {
	PolicyName string     `json:"policy_name"`
	ScopeKind  string     `json:"scope_kind"`
	ScopeID    uuid.UUID  `json:"scope_id"`
	Algorithm  string     `json:"algorithm"`
	LocalLimit int64      `json:"local_limit"`
	DurationMs int64      `json:"duration_ms"`
	Limit      *int64     `json:"limit,omitempty"`
	Remaining  *int64     `json:"remaining,omitempty"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
	Cost       *int64     `json:"cost,omitempty"`
	LocalCost  int64      `json:"local_cost"`
}

type SyncRequest struct {
	AccountID        uuid.UUID
	ServiceVersionID uuid.UUID
	CooldownUntil    *time.Time
	Observations     []ResponseObservation
}

const ProviderRateLimitStateSchemaVersion = 1
const MaxRollingStateBuckets = 256

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
	Name                 string                       `json:"name"`
	ScopeKind            string                       `json:"scope_kind"`
	ScopeID              uuid.UUID                    `json:"scope_id"`
	ConfigHash           string                       `json:"config_hash"`
	Algorithm            string                       `json:"algorithm"`
	FixedWindowStartedAt *time.Time                   `json:"fixed_window_started_at,omitempty"`
	FixedWindowUsed      int64                        `json:"fixed_window_used"`
	Tokens               *int64                       `json:"tokens,omitempty"`
	TokenRefilledAt      *time.Time                   `json:"token_refilled_at,omitempty"`
	RollingUsage         []RollingUsage               `json:"rolling_usage,omitempty"`
	ConcurrencyUsed      int64                        `json:"concurrency_used"`
	ConcurrencyHolders   map[string]ConcurrencyHolder `json:"concurrency_holders,omitempty"`
}

type ConcurrencyHolder struct {
	Cost      int64     `json:"cost"`
	ExpiresAt time.Time `json:"expires_at"`
}

type RollingUsage struct {
	At   time.Time `json:"at"`
	Cost int64     `json:"cost"`
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
	if !validAlgorithmState(s) {
		return errors.New("provider rate-limit policy algorithm state is invalid")
	}
	return nil
}

func validAlgorithmState(s PolicyState) bool {
	switch s.Algorithm {
	case "fixed_window":
		return s.FixedWindowStartedAt != nil && noTokenState(s)
	case "token_bucket":
		return validTokenState(s)
	case "rolling_window":
		return s.FixedWindowStartedAt == nil && noTokenState(s)
	case "concurrency":
		return s.FixedWindowStartedAt == nil && noTokenState(s) && len(s.RollingUsage) == 0
	default:
		return false
	}
}

func noTokenState(s PolicyState) bool { return s.Tokens == nil && s.TokenRefilledAt == nil }

func (s PolicyState) validateIdentity() error {
	if !s.validBaseIdentity() {
		return errors.New("provider rate-limit policy state is invalid")
	}
	if !s.validUsageBounds() {
		return errors.New("provider rate-limit policy dynamic state is invalid")
	}
	if err := validateRollingUsage(s.RollingUsage); err != nil {
		return err
	}
	return validateConcurrencyHolders(s.ConcurrencyHolders, s.ConcurrencyUsed)
}

func (s PolicyState) validBaseIdentity() bool {
	return s.Name != "" && s.ScopeID != uuid.Nil && s.ConfigHash != "" && s.ScopeKind != "" && len(s.ScopeKind) <= 256
}

func (s PolicyState) validUsageBounds() bool {
	return s.FixedWindowUsed >= 0 && s.FixedWindowUsed <= maxPolicyValue && s.ConcurrencyUsed >= 0 && s.ConcurrencyUsed <= maxPolicyValue && len(s.RollingUsage) <= MaxRollingStateBuckets && len(s.ConcurrencyHolders) <= 10_000
}

func validateRollingUsage(values []RollingUsage) error {
	var previous time.Time
	for _, usage := range values {
		if usage.At.IsZero() || usage.Cost < 0 || usage.Cost > maxPolicyValue {
			return errors.New("provider rolling-window state is invalid")
		}
		if !previous.IsZero() && !usage.At.After(previous) {
			return errors.New("provider rolling-window state is unordered")
		}
		previous = usage.At
	}
	return nil
}

func validateConcurrencyHolders(values map[string]ConcurrencyHolder, expected int64) error {
	holderTotal := int64(0)
	for _, holder := range values {
		if holder.Cost < 1 || holder.Cost > maxPolicyValue || holder.ExpiresAt.IsZero() {
			return errors.New("provider concurrency holder is invalid")
		}
		if holder.Cost > maxPolicyValue-holderTotal {
			return errors.New("provider concurrency holder total overflowed")
		}
		holderTotal += holder.Cost
	}
	if holderTotal != expected {
		return errors.New("provider concurrency usage is inconsistent")
	}
	return nil
}

func validTokenState(state PolicyState) bool {
	return state.FixedWindowStartedAt == nil && state.FixedWindowUsed == 0 &&
		state.Tokens != nil && *state.Tokens >= 0 && *state.Tokens <= maxPolicyValue &&
		state.TokenRefilledAt != nil
}
