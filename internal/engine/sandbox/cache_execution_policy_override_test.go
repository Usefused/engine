package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// executionPolicyOverrideStubStore embeds store.Store so it satisfies the
// broad interface LocalObjectCache.db is typed as, while only implementing
// the one method applyExecutionPolicyOverride actually looks for via type
// assertion -- the same pattern the real postgresStore uses in production.
type executionPolicyOverrideStubStore struct {
	store.Store
	override *store.WorkspaceExecutionPolicyOverride
	err      error
}

func (s *executionPolicyOverrideStubStore) GetEffectiveWorkspaceExecutionPolicyOverride(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceExecutionPolicyOverride, error) {
	return s.override, s.err
}

// executionPolicyOverrideNarrowStore does NOT implement
// GetEffectiveWorkspaceExecutionPolicyOverride, mirroring a test double that
// hasn't been updated for this capability -- applyExecutionPolicyOverride
// must degrade to a no-op rather than panic on such a store.
type executionPolicyOverrideNarrowStore struct {
	store.Store
}

func snapshotMetadataFixture() *fusedobject.ServiceMetadata {
	timeoutMs := 30000
	return &fusedobject.ServiceMetadata{
		RateLimit:           &fusedobject.RateLimitConfig{Strategy: "fixed_window", RequestsPerSecond: 100},
		TimeoutMs:           &timeoutMs,
		EventExtractionPath: "body.type",
	}
}

func TestApplyExecutionPolicyOverride_NoOverride_ReturnsSnapshotUnchanged(t *testing.T) {
	c := &LocalObjectCache{db: &executionPolicyOverrideStubStore{override: nil}}
	metadata := snapshotMetadataFixture()

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), metadata)

	if got.RateLimit.RequestsPerSecond != 100 || got.EventExtractionPath != "body.type" {
		t.Fatalf("expected snapshot values unchanged, got %#v", got)
	}
}

func TestApplyExecutionPolicyOverride_FieldPresent_WinsOverSnapshot(t *testing.T) {
	timeoutMs := 5000
	c := &LocalObjectCache{db: &executionPolicyOverrideStubStore{
		override: &store.WorkspaceExecutionPolicyOverride{
			RateLimit: &fusedobject.RateLimitConfig{Strategy: "token_bucket", RequestsPerSecond: 5},
			TimeoutMs: &timeoutMs,
		},
	}}
	metadata := snapshotMetadataFixture()

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), metadata)

	if got.RateLimit.RequestsPerSecond != 5 {
		t.Fatalf("expected override rate_limit to win, got %#v", got.RateLimit)
	}
	if got.TimeoutMs == nil || *got.TimeoutMs != timeoutMs {
		t.Fatalf("expected override timeout_ms to win, got %v", got.TimeoutMs)
	}
	// A field the override didn't set must still fall through to the snapshot
	// value -- this is a per-field merge, not a whole-row replacement.
	if got.EventExtractionPath != "body.type" {
		t.Fatalf("expected unset override field to fall through to snapshot value, got %q", got.EventExtractionPath)
	}
}

func TestApplyExecutionPolicyOverride_EventExtractionPath_OnlyThatFieldOverridden(t *testing.T) {
	overriddenPath := "body.event_type"
	c := &LocalObjectCache{db: &executionPolicyOverrideStubStore{
		override: &store.WorkspaceExecutionPolicyOverride{
			EventExtractionPath: &overriddenPath,
		},
	}}
	metadata := snapshotMetadataFixture()

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), metadata)

	if got.EventExtractionPath != overriddenPath {
		t.Fatalf("expected event_extraction_path override to win, got %q", got.EventExtractionPath)
	}
	if got.RateLimit.RequestsPerSecond != 100 {
		t.Fatalf("expected rate_limit to remain the snapshot value, got %#v", got.RateLimit)
	}
}

func TestApplyExecutionPolicyOverride_LookupError_SoftFailsToSnapshot(t *testing.T) {
	c := &LocalObjectCache{db: &executionPolicyOverrideStubStore{err: errors.New("boom")}}
	metadata := snapshotMetadataFixture()

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), metadata)

	if got.RateLimit.RequestsPerSecond != 100 {
		t.Fatalf("expected snapshot value on lookup error, got %#v", got)
	}
}

func TestApplyExecutionPolicyOverride_StoreWithoutCapability_NoOp(t *testing.T) {
	c := &LocalObjectCache{db: &executionPolicyOverrideNarrowStore{}}
	metadata := snapshotMetadataFixture()

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), metadata)

	if got.RateLimit.RequestsPerSecond != 100 {
		t.Fatalf("expected snapshot value when store lacks the override capability, got %#v", got)
	}
}

func TestApplyExecutionPolicyOverride_NilMetadata_ReturnsNil(t *testing.T) {
	c := &LocalObjectCache{db: &executionPolicyOverrideStubStore{}}

	got := c.applyExecutionPolicyOverride(context.Background(), uuid.New(), uuid.New(), nil)

	if got != nil {
		t.Fatalf("expected nil metadata to pass through as nil, got %#v", got)
	}
}
