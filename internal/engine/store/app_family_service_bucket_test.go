package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// appFamilyServiceBucketFixture bundles a connected postgresStore plus a
// fresh app family and two workspace-service rows so per-service bucket
// override tests can bind services without repeating setup boilerplate.
type appFamilyServiceBucketFixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	repository *postgresStore
	familyID   uuid.UUID
	serviceA   uuid.UUID
	serviceB   uuid.UUID
}

// newAppFamilyServiceBucketFixture connects to the local integration
// database (skipping the test when DATABASE_URL is unset, matching the
// other store integration tests), then seeds a family and two services so
// override tests have distinct FK targets to bind buckets against.
func newAppFamilyServiceBucketFixture(t *testing.T) appFamilyServiceBucketFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	// Integration tests against a real Postgres instance are opt-in via
	// DATABASE_URL, same convention as the rest of this package.
	if dbURL == "" {
		t.Skip("DATABASE_URL is required for app family service bucket integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		cancel()
		t.Fatalf("initialize Engine database: %v", err)
	}
	t.Cleanup(cancel)
	t.Cleanup(pool.Close)

	accountID := uuid.New()
	ownerTeamID := seedAppOwnerTeam(t, ctx, pool)
	familyID := uuid.New()
	name := "svc-bucket-" + familyID.String()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_app_families (app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Service bucket test', 'typescript', $4)
	`, familyID, accountID, name, ownerTeamID); err != nil {
		t.Fatalf("seed app family: %v", err)
	}

	serviceA, serviceB := uuid.New(), uuid.New()
	for _, serviceID := range []uuid.UUID{serviceA, serviceB} {
		// Minimal service row satisfying the service_id FK on
		// fused_app_family_buckets; slug/name are irrelevant to this test.
		if _, err := pool.Exec(ctx, `
			INSERT INTO fused_workspace_services (service_id, service_slug, service_name)
			VALUES ($1, $2, $2)
		`, serviceID, "svc-"+serviceID.String()); err != nil {
			t.Fatalf("seed workspace service: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM fused_app_family_buckets WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM fused_app_families WHERE app_family_id = $1`, familyID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM fused_workspace_services WHERE service_id IN ($1, $2)`, serviceA, serviceB)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM fused_teams WHERE id = $1`, ownerTeamID)
	})

	return appFamilyServiceBucketFixture{
		ctx:        ctx,
		pool:       pool,
		repository: NewPostgresStore(pool).(*postgresStore),
		familyID:   familyID,
		serviceA:   serviceA,
		serviceB:   serviceB,
	}
}

// TestResolveAppFamilyServiceBucketFallsBackToDefault verifies that a
// service without its own override resolves through the family's default
// bucket, and that setting an override for a different service does not
// disturb that fallback.
func TestResolveAppFamilyServiceBucketFallsBackToDefault(t *testing.T) {
	fixture := newAppFamilyServiceBucketFixture(t)

	defaultBucket, err := fixture.repository.CreateBucket(fixture.ctx, "default-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("create default bucket: %v", err)
	}
	overrideBucket, err := fixture.repository.CreateBucket(fixture.ctx, "override-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("create override bucket: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.repository.db.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id IN ($1, $2)`, defaultBucket.ID, overrideBucket.ID)
	})

	if _, err := fixture.repository.db.Exec(fixture.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $2)
	`, fixture.familyID, defaultBucket.ID); err != nil {
		t.Fatalf("set family default bucket: %v", err)
	}
	// Only serviceA gets an override; serviceB must keep resolving to the default.
	if err := fixture.repository.SetAppFamilyServiceBucket(fixture.ctx, fixture.familyID, fixture.serviceA, overrideBucket.ID); err != nil {
		t.Fatalf("set service override: %v", err)
	}

	resolvedA, err := fixture.repository.ResolveAppFamilyServiceBucket(fixture.ctx, fixture.familyID, fixture.serviceA)
	if err != nil || resolvedA.BucketID != overrideBucket.ID {
		t.Fatalf("resolve serviceA: bucket=%#v err=%v want=%s", resolvedA, err, overrideBucket.ID)
	}
	resolvedB, err := fixture.repository.ResolveAppFamilyServiceBucket(fixture.ctx, fixture.familyID, fixture.serviceB)
	if err != nil || resolvedB.BucketID != defaultBucket.ID {
		t.Fatalf("resolve serviceB fallback: bucket=%#v err=%v want=%s", resolvedB, err, defaultBucket.ID)
	}

	overrides, err := fixture.repository.ListAppFamilyServiceBuckets(fixture.ctx, fixture.familyID)
	if err != nil {
		t.Fatalf("list overrides: %v", err)
	}
	// The default row must never appear in the per-service override listing.
	if len(overrides) != 1 || overrides[fixture.serviceA].BucketID != overrideBucket.ID {
		t.Fatalf("list overrides = %#v, want exactly one entry for serviceA", overrides)
	}

	if _, err := fixture.repository.db.Exec(fixture.ctx, `
		DELETE FROM fused_app_family_buckets
		WHERE app_family_id = $1 AND service_id = $2
	`, fixture.familyID, fixture.serviceA); err != nil {
		t.Fatalf("delete override: %v", err)
	}
	revertedA, err := fixture.repository.ResolveAppFamilyServiceBucket(fixture.ctx, fixture.familyID, fixture.serviceA)
	if err != nil || revertedA.BucketID != defaultBucket.ID {
		t.Fatalf("resolve serviceA after delete: bucket=%#v err=%v want=%s", revertedA, err, defaultBucket.ID)
	}
}

// TestSetAppFamilyServiceBucketRejectsSecondDefaultRow confirms the partial
// unique index still allows exactly one service_id IS NULL row per family
// even after per-service override rows exist -- a second direct insert of a
// default row must fail rather than silently creating a duplicate default.
func TestSetAppFamilyServiceBucketRejectsSecondDefaultRow(t *testing.T) {
	fixture := newAppFamilyServiceBucketFixture(t)

	bucketOne, err := fixture.repository.CreateBucket(fixture.ctx, "one-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("create bucket one: %v", err)
	}
	bucketTwo, err := fixture.repository.CreateBucket(fixture.ctx, "two-"+uuid.NewString(), false)
	if err != nil {
		t.Fatalf("create bucket two: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.repository.db.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id IN ($1, $2)`, bucketOne.ID, bucketTwo.ID)
	})

	if _, err := fixture.repository.db.Exec(fixture.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $2)
	`, fixture.familyID, bucketOne.ID); err != nil {
		t.Fatalf("set first default bucket: %v", err)
	}
	// A second direct insert of a default row proves the schema-level
	// invariant itself: the partial unique index rejects it.
	_, err = fixture.repository.db.Exec(fixture.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $2)
	`, fixture.familyID, bucketTwo.ID)
	if err == nil {
		t.Fatal("expected a second default (service_id IS NULL) row to violate the partial unique index, got nil error")
	}

	// The surviving default row must still resolve through the runtime lookup,
	// proving the rejected duplicate left exactly one binding in place.
	resolved, err := fixture.repository.ResolveAppFamilyServiceBucket(fixture.ctx, fixture.familyID, fixture.serviceA)
	if err != nil || resolved.BucketID != bucketOne.ID {
		t.Fatalf("resolve default bucket after rejected duplicate: bucket=%#v err=%v want=%s", resolved, err, bucketOne.ID)
	}
}
