package ratelimitpolicy

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestStateEnvelopeValidate(t *testing.T) {
	now := time.Now().UTC()
	tokens := int64(3)
	valid := []PolicyState{
		{Name: "fixed", ScopeKind: "service_version", ScopeID: uuid.New(), ConfigHash: "a", Algorithm: "fixed_window", FixedWindowStartedAt: &now},
		{Name: "token", ScopeKind: "connection", ScopeID: uuid.New(), ConfigHash: "b", Algorithm: "token_bucket", Tokens: &tokens, TokenRefilledAt: &now},
	}
	for _, policy := range valid {
		state := StateEnvelope{
			SchemaVersion: ProviderRateLimitStateSchemaVersion,
			AccountID:     uuid.New(), ServiceVersionID: uuid.New(), Policies: []PolicyState{policy}, UpdatedAt: now,
		}
		if err := state.Validate(); err != nil {
			t.Fatalf("valid %s state: %v", policy.Algorithm, err)
		}
	}
}

func TestStateEnvelopeRejectsMixedAlgorithmState(t *testing.T) {
	now := time.Now().UTC()
	tokens := int64(1)
	state := StateEnvelope{
		SchemaVersion: ProviderRateLimitStateSchemaVersion,
		AccountID:     uuid.New(), ServiceVersionID: uuid.New(), UpdatedAt: now,
		Policies: []PolicyState{{
			Name: "invalid", ScopeKind: "connection", ScopeID: uuid.New(), ConfigHash: "config",
			Algorithm: "fixed_window", FixedWindowStartedAt: &now, Tokens: &tokens,
		}},
	}
	if err := state.Validate(); err == nil {
		t.Fatal("mixed fixed-window/token state should be rejected")
	}
}
