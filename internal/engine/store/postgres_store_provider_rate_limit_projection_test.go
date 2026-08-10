package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

func TestProviderRateLimitProjectionSQLIsSetBasedAndMonotonic(t *testing.T) {
	for _, fragment := range []string{
		"jsonb_to_recordset($1::jsonb)",
		"CROSS JOIN LATERAL jsonb_to_recordset(envelope.policies)",
		"DISTINCT ON (account_id, service_version_id, policy_name, scope_kind, scope_id)",
		"ON CONFLICT (account_id, service_version_id, policy_name, scope_kind, scope_id)",
		"WHERE EXCLUDED.state_sequence > current.state_sequence",
	} {
		if !strings.Contains(upsertProviderRateLimitProjectionSQL, fragment) {
			t.Fatalf("projection SQL is missing %q", fragment)
		}
	}
}

func TestProviderRateLimitProjectionPersistsLatestBatchAndRecovers(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)

	request := projectionRequest()
	newer := projectionState(request, time.Now().UTC().Truncate(time.Microsecond), 3)
	older := projectionState(request, newer.UpdatedAt.Add(-time.Minute), 1)
	if err := repository.BatchUpsertProviderRateLimitStates(ctx, []ratelimitpolicy.StateEnvelope{newer, older}); err != nil {
		t.Fatalf("persist projection: %v", err)
	}

	recovered, err := repository.LoadProviderRateLimitState(ctx, request)
	if err != nil {
		t.Fatalf("recover projection: %v", err)
	}
	if recovered == nil || len(recovered.Policies) != 2 {
		t.Fatalf("recovered = %#v, want two policies", recovered)
	}
	for _, policy := range recovered.Policies {
		if policy.FixedWindowUsed != 3 {
			t.Fatalf("policy %q used = %d, want latest value 3", policy.Name, policy.FixedWindowUsed)
		}
	}
	if !recovered.UpdatedAt.Equal(newer.UpdatedAt) {
		t.Fatalf("updated_at = %s, want %s", recovered.UpdatedAt, newer.UpdatedAt)
	}
	if recovered.Sequence != newer.Sequence {
		t.Fatalf("sequence = %d, want %d", recovered.Sequence, newer.Sequence)
	}
}

func projectionRequest() ratelimitpolicy.AcquireRequest {
	versionID := uuid.New()
	scopeID := uuid.New()
	return ratelimitpolicy.AcquireRequest{
		AccountID: uuid.New(), ServiceVersionID: versionID,
		Policies: []ratelimitpolicy.ResolvedPolicy{
			{Name: "minute", ScopeKind: "connection", ScopeID: scopeID.String(), Cost: 1, Algorithm: "fixed_window", ConfigHash: "minute", Limit: 10, DurationMS: 60_000},
			{Name: "second", ScopeKind: "service_version", ScopeID: versionID.String(), Cost: 1, Algorithm: "fixed_window", ConfigHash: "second", Limit: 2, DurationMS: 1_000},
		},
	}
}

func projectionState(request ratelimitpolicy.AcquireRequest, updatedAt time.Time, used int64) ratelimitpolicy.StateEnvelope {
	policies := make([]ratelimitpolicy.PolicyState, len(request.Policies))
	for index, policy := range request.Policies {
		startedAt := updatedAt.Add(-time.Second)
		policies[index] = ratelimitpolicy.PolicyState{
			Name: policy.Name, ScopeKind: policy.ScopeKind, ScopeID: uuid.MustParse(policy.ScopeID),
			ConfigHash: policy.ConfigHash, Algorithm: policy.Algorithm,
			FixedWindowStartedAt: &startedAt, FixedWindowUsed: used,
		}
	}
	return ratelimitpolicy.StateEnvelope{
		SchemaVersion: ratelimitpolicy.ProviderRateLimitStateSchemaVersion,
		AccountID:     request.AccountID, ServiceVersionID: request.ServiceVersionID,
		Policies: policies, Sequence: uint64(used), UpdatedAt: updatedAt,
	}
}
