package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestPostgresStore_ArtifactScopeBucketResolution covers GetArtifactScope and
// ListArtifactScopes's bucket_id resolution after the fused_artifact_scopes.bucket_id
// column was dropped in favor of the fused_artifact_buckets join table. Both
// functions independently select bucket_id, so both need their own coverage
// -- ListArtifactScopes previously still selected bucket_id directly off
// fused_artifact_scopes (same "column does not exist" failure GetArtifactScope had,
// just a second call site) until this fix.
func TestPostgresStore_ArtifactScopeBucketResolution(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	f := newArtifactScopeBucketFixture(t, ctx, dbURL)

	t.Run("GetArtifactScope resolves the linked bucket", func(t *testing.T) {
		assertLinkedArtifactScopeBucket(t, f)
	})

	t.Run("GetArtifactScope leaves BucketID as the zero value when nothing is linked", func(t *testing.T) {
		assertUnlinkedArtifactScopeBucket(t, f)
	})

	t.Run("ListArtifactScopes resolves bucket_id via the join for every requested SDK", func(t *testing.T) {
		assertBatchArtifactScopeBuckets(t, f)
	})

	t.Run("ListArtifactScopesForBucket uses the shared artifact scope projection", func(t *testing.T) {
		assertBucketArtifactScopePage(t, f)
	})
}

type artifactScopeBucketFixture struct {
	ctx              context.Context
	store            *postgresStore
	accountID        uuid.UUID
	bucketID         uuid.UUID
	secondBucketID   uuid.UUID
	sdkWithBucket    uuid.UUID
	sdkWithoutBucket uuid.UUID
}

func newArtifactScopeBucketFixture(t *testing.T, ctx context.Context, dbURL string) artifactScopeBucketFixture {
	t.Helper()
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)

	f := artifactScopeBucketFixture{
		ctx:              ctx,
		store:            NewPostgresStore(pool).(*postgresStore),
		accountID:        uuid.New(),
		bucketID:         uuid.New(),
		secondBucketID:   uuid.New(),
		sdkWithBucket:    uuid.New(),
		sdkWithoutBucket: uuid.New(),
	}
	cleanAndSeedArtifactScopeBuckets(t, f)
	seedArtifactScopes(t, f)
	linkArtifactScopeBucket(t, f)
	return f
}

func cleanAndSeedArtifactScopeBuckets(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	workspaceID := uuid.New()
	if _, err := f.store.db.Exec(f.ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("clean workspace: %v", err)
	}
	if _, err := f.store.db.Exec(f.ctx, "DELETE FROM fused_buckets"); err != nil {
		t.Fatalf("clean buckets: %v", err)
	}
	if _, err := f.store.db.Exec(f.ctx, "INSERT INTO fused_workspaces (id, account_id, name, slug) VALUES ($1, $2, $3, $4)", workspaceID, f.accountID, "Test WS", "test-ws"); err != nil {
		t.Fatalf("setup workspace failed: %v", err)
	}
	if _, err := f.store.db.Exec(f.ctx, "INSERT INTO fused_buckets (id, name) VALUES ($1, $2)", f.bucketID, "test-bucket-"+uuid.NewString()); err != nil {
		t.Fatalf("setup bucket failed: %v", err)
	}
	if _, err := f.store.db.Exec(f.ctx, "INSERT INTO fused_buckets (id, name) VALUES ($1, $2)", f.secondBucketID, "second-test-bucket-"+uuid.NewString()); err != nil {
		t.Fatalf("setup second bucket failed: %v", err)
	}
}

func seedArtifactScopes(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	for _, scope := range artifactScopeBucketFixtureScopes(f) {
		if err := f.store.SaveArtifactScope(f.ctx, scope); err != nil {
			t.Fatalf("SaveArtifactScope(%s) failed: %v", scope.ArtifactID, err)
		}
	}
}

func artifactScopeBucketFixtureScopes(f artifactScopeBucketFixture) []ArtifactScope {
	return []ArtifactScope{
		{
			AccountID:          f.accountID,
			ArtifactID:         f.sdkWithBucket,
			Selections:         []byte("[]"),
			ScopeSchemaVersion: 1,
			Name:               "prod sdk",
			Version:            "1.0.0",
			ConfigKey:          "sdk:prod:1.0.0",
		},
		{
			AccountID:          f.accountID,
			ArtifactID:         f.sdkWithoutBucket,
			Selections:         []byte("[]"),
			ScopeSchemaVersion: 1,
		},
	}
}

func linkArtifactScopeBucket(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	// The unlinked SDK below exercises the LEFT JOIN's NULL case, proving
	// missing bucket rows resolve to zero UUIDs rather than being filtered out.
	if err := f.store.LinkBucketToSDK(f.ctx, f.sdkWithBucket, f.bucketID); err != nil {
		t.Fatalf("LinkBucketToSDK failed: %v", err)
	}
	if err := f.store.LinkBucketToSDK(f.ctx, f.sdkWithBucket, f.bucketID); err != nil {
		t.Fatalf("idempotent LinkBucketToSDK failed: %v", err)
	}
	if err := f.store.LinkBucketToSDK(f.ctx, f.sdkWithBucket, f.secondBucketID); !errors.Is(err, ErrSDKBucketImmutable) {
		t.Fatalf("second bucket link error = %v, want ErrSDKBucketImmutable", err)
	}
}

func assertLinkedArtifactScopeBucket(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	scope, err := f.store.GetArtifactScope(f.ctx, f.sdkWithBucket)
	if err != nil {
		t.Fatalf("GetArtifactScope failed: %v", err)
	}
	if scope.BucketID != f.bucketID {
		t.Errorf("BucketID = %v, want %v", scope.BucketID, f.bucketID)
	}
}

func assertUnlinkedArtifactScopeBucket(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	scope, err := f.store.GetArtifactScope(f.ctx, f.sdkWithoutBucket)
	if err != nil {
		t.Fatalf("GetArtifactScope failed: %v", err)
	}
	if scope.BucketID != uuid.Nil {
		t.Errorf("BucketID = %v, want uuid.Nil", scope.BucketID)
	}
}

func assertBatchArtifactScopeBuckets(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	scopes, err := f.store.ListArtifactScopes(f.ctx, []uuid.UUID{f.sdkWithBucket, f.sdkWithoutBucket})
	if err != nil {
		t.Fatalf("ListArtifactScopes failed: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2", len(scopes))
	}
	if got := scopes[f.sdkWithBucket].BucketID; got != f.bucketID {
		t.Errorf("scopes[sdkWithBucket].BucketID = %v, want %v", got, f.bucketID)
	}
	if got := scopes[f.sdkWithoutBucket].BucketID; got != uuid.Nil {
		t.Errorf("scopes[sdkWithoutBucket].BucketID = %v, want uuid.Nil", got)
	}
}

func assertBucketArtifactScopePage(t *testing.T, f artifactScopeBucketFixture) {
	t.Helper()
	scopes, total, err := f.store.ListArtifactScopesForBucket(f.ctx, f.bucketID, 10, 0)
	if err != nil {
		t.Fatalf("ListArtifactScopesForBucket failed: %v", err)
	}
	assertBucketArtifactScopePageShape(t, scopes, total)
	assertBucketArtifactScopeMetadata(t, scopes[0], f)
}

func assertBucketArtifactScopePageShape(t *testing.T, scopes []ArtifactScope, total int) {
	t.Helper()
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(scopes) != 1 {
		t.Fatalf("len(scopes) = %d, want 1", len(scopes))
	}
}

func assertBucketArtifactScopeMetadata(t *testing.T, scope ArtifactScope, f artifactScopeBucketFixture) {
	t.Helper()
	if scope.ArtifactID != f.sdkWithBucket {
		t.Fatalf("ArtifactID = %v, want %v", scope.ArtifactID, f.sdkWithBucket)
	}
	if scope.BucketID != f.bucketID {
		t.Errorf("BucketID = %v, want %v", scope.BucketID, f.bucketID)
	}
	if scope.Name != "prod sdk" || scope.Version != "1.0.0" || scope.ConfigKey != "sdk:prod:1.0.0" {
		t.Errorf("metadata = (%q, %q, %q), want prod sdk/1.0.0/sdk:prod:1.0.0", scope.Name, scope.Version, scope.ConfigKey)
	}
}
