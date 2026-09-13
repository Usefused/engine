package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
)

// TestWorkspaceApplyRefreshesSecretCache verifies committed bucket secrets replace cached misses and old values through real PostgreSQL transactions.
func TestWorkspaceApplyRefreshesSecretCache(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// Schema isolation still requires an explicitly selected test database.
	if dsn == "" {
		t.Skip("DATABASE_URL required for PostgreSQL secret-cache test")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	s := NewCachedStore(NewPostgresStore(pool), nil).(*cachedStore)
	accountID := uuid.New()
	// Bootstrap establishes the actual bucket and workspace invariants used by apply.
	if _, err := s.BootstrapWorkspace(ctx, accountID, "Secret cache fixture"); err != nil {
		t.Fatal(err)
	}
	bucketID, err := s.LoadDefaultBucketID(ctx)
	// Generic webhook secrets must stay bound to the real default bucket identity.
	if err != nil {
		t.Fatal(err)
	}
	repo := NewPostgresConfigRepository(pool)
	for _, name := range []string{"missing", "rotation"} {
		secret := WorkspaceSecret{WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: bucketID, KeyName: "secret:" + name, CredentialType: "bucket_secret"}, EncryptedDEK: "test-wrapped-key", EncryptedValue: "old-ciphertext"}
		// Rotation starts with a cached hit; creation starts with a cached absence.
		if name == "rotation" {
			// Seed through the ordinary write before warming the cache.
			if err := s.UpsertSecret(ctx, secret); err != nil {
				t.Fatal(err)
			}
		}
		// Warm the exact lookup used by inbound webhook verification.
		if _, err := s.GetSecret(ctx, bucketID, uuid.Nil, secret.KeyName); err != nil {
			t.Fatal(err)
		}
		plan, err := repo.CreateConfigPlan(ctx, CreateConfigPlanParams{ConfigKey: "workspace:" + name, ConfigType: ConfigTypeWorkspace, SourceHash: "sha256:fixture", Actions: json.RawMessage(`[]`), DesiredState: json.RawMessage(`{}`), ResolvedPayload: json.RawMessage(`{}`), RequiredPermissions: testRequiredPermissions(accountID), CreatedBy: accountID})
		// Use real plan admission and lease ownership rather than invoking a raw SQL write.
		if err != nil {
			t.Fatal(err)
		}
		lease, err := repo.ReserveConfigPlanApply(ctx, plan.ID, plan.Revision)
		// No transactional mutation may proceed without an admitted lease.
		if err != nil {
			t.Fatal(err)
		}
		secret.EncryptedValue = "new-ciphertext"
		// Bucket apply writes through its transaction-bound store, bypassing individual cached-store upserts.
		mutation := func(ctx context.Context, txStore Store, _ ConfigRepository, _ *ConfigState) (*UpsertConfigStateParams, error) {
			// Secret persistence and the success receipt must commit together.
			if err := txStore.UpsertSecrets(ctx, []WorkspaceSecret{secret}); err != nil {
				return nil, err
			}
			return &UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: ConfigTypeWorkspace, SourceHash: "sha256:fixture", DesiredState: json.RawMessage(`{}`), ManagedResources: json.RawMessage(`{}`), UpdatedBy: accountID}, nil
		}
		// Successful apply must invalidate stale credential entries before returning to its caller.
		if err := s.RunWorkspaceApplyStep(ctx, WorkspaceApplyStep{PlanID: plan.ID, Revision: plan.Revision, LeaseID: lease.ID, Key: "buckets"}, mutation); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetSecret(ctx, bucketID, uuid.Nil, secret.KeyName)
		// A five-minute TTL is unacceptable after the user receives a successful apply receipt.
		if err != nil || got == nil || got.EncryptedValue != "new-ciphertext" {
			t.Fatalf("%s retained stale secret after commit: secret=%v err=%v", name, got != nil, err)
		}
	}
}
