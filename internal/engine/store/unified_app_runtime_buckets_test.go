package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type runtimeBucketDBFixture struct {
	base                                concurrentArtifactApplyFixture
	repository                          *postgresStore
	appID, accountID, tokenID, bucketID uuid.UUID
}

// newRuntimeBucketDBFixture publishes real immutable scope and gives its owning team one exact bucket grant.
func newRuntimeBucketDBFixture(t *testing.T) runtimeBucketDBFixture {
	t.Helper()
	base := newConcurrentArtifactApplyFixture(t, ConfigTypeUnifiedApp)
	f := runtimeBucketDBFixture{base: base, repository: NewPostgresStore(base.pool).(*postgresStore), appID: base.params.Scope.AppID, accountID: base.params.Scope.AccountID, bucketID: uuid.New()}
	f.exec(t, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, f.bucketID, "runtime-"+f.bucketID.String())
	payload, _ := json.Marshal(map[string]any{"runtime_buckets": []map[string]any{{"bucket_id": f.bucketID, "bucket_name": "runtime-" + f.bucketID.String()}}})
	f.exec(t, `UPDATE fused_config_plans SET resolved_payload=$2 WHERE id=$1`, base.params.Plan.PlanID, payload)
	f.exec(t, `INSERT INTO fused_role_bindings(subject_type,subject_id,role_id,resource_type,resource_id) SELECT 'team',$1,id,'bucket',$2 FROM fused_roles WHERE slug='bucket-user'`, base.params.Scope.OwnerTeamID, f.bucketID)
	bundle := UnifiedAppBundle{AppID: f.appID, SourceHash: base.params.Plan.State.SourceHash, BundleJS: "globalThis.FusedUnifiedApp={};", Manifest: json.RawMessage(`{"schemaVersion":1}`)}
	base.params.Scope.BundleDigest = UnifiedAppBundleDigest([]byte(bundle.BundleJS))
	base.params.ExecutionBundle = &bundle
	base.params.TokenHash = "runtime-bucket-" + uuid.NewString()
	// Real publication must commit both the applied plan and the current traffic pointer.
	if _, err := base.repository.ApplyAppConfigPlan(base.ctx, base.params); err != nil {
		t.Fatal(err)
	}
	if err := base.pool.QueryRow(base.ctx, `SELECT id FROM fused_app_tokens WHERE token_hash=$1`, base.params.TokenHash).Scan(&f.tokenID); err != nil {
		t.Fatal(err)
	}
	return f
}

// exec keeps test setup mutations explicit and fails before evaluating a partially seeded security state.
func (f runtimeBucketDBFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.base.pool.Exec(f.base.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

// expect checks the real uncached SQL boundary using only opaque identities in diagnostics.
func (f runtimeBucketDBFixture) expect(t *testing.T, want bool) {
	t.Helper()
	got, err := f.repository.AuthorizeUnifiedAppRuntimeBucket(f.base.ctx, f.accountID, f.appID, f.tokenID, f.bucketID)
	if err != nil || got != want {
		t.Fatalf("allowed=%t want=%t err=%v", got, want, err)
	}
}

// TestRuntimeBucketPostgresAuthorization proves exact scope, live grants and live token checks against PostgreSQL.
func TestRuntimeBucketPostgresAuthorization(t *testing.T) {
	f := newRuntimeBucketDBFixture(t)
	f.expect(t, true)
	for _, mutation := range []string{
		`UPDATE fused_app_tokens SET expires_at=NOW()-interval '1 second' WHERE id=$1`,
		`UPDATE fused_app_tokens SET binding_mode='fixed' WHERE id=$1`,
		`UPDATE fused_app_tokens SET allow_all=false,allowed_operations=ARRAY['other'] WHERE id=$1`,
	} {
		f.exec(t, mutation, f.tokenID)
		f.expect(t, false)
		f.exec(t, `UPDATE fused_app_tokens SET expires_at=NULL,binding_mode='dynamic',allow_all=true,allowed_operations='{}' WHERE id=$1`, f.tokenID)
	}
	other := f
	other.bucketID = uuid.New()
	other.expect(t, false)
	other = f
	other.accountID = uuid.New()
	other.expect(t, false)
	other = f
	other.tokenID = uuid.New()
	other.expect(t, false)
	f.exec(t, `DELETE FROM fused_role_bindings WHERE subject_type='team' AND subject_id=$1 AND resource_id=$2`, f.base.params.Scope.OwnerTeamID, f.bucketID)
	f.expect(t, false)
}

// TestRuntimeBucketPostgresPolicyAndDeletion ensures deployment opt-in and immutable IDs cannot be bypassed by names.
func TestRuntimeBucketPostgresPolicyAndDeletion(t *testing.T) {
	f := newRuntimeBucketDBFixture(t)
	buckets, err := f.repository.ReadUnifiedAppRuntimeBuckets(f.base.ctx, f.accountID, f.appID)
	// Public schema projection must retain the same identity that runtime authorization checks.
	if err != nil || len(buckets) != 1 || buckets[0].ID != f.bucketID {
		t.Fatalf("scope=%v err=%v", buckets, err)
	}
	f.exec(t, `UPDATE fused_config_plans SET resolved_payload='{}' WHERE id=$1`, f.base.params.Plan.PlanID)
	f.expect(t, false)
	payload, _ := json.Marshal(map[string]any{"runtime_buckets": []map[string]any{{"bucket_id": f.bucketID, "bucket_name": buckets[0].Name}}})
	f.exec(t, `UPDATE fused_config_plans SET resolved_payload=$2 WHERE id=$1`, f.base.params.Plan.PlanID, payload)
	f.expect(t, true)
	f.exec(t, `DELETE FROM fused_buckets WHERE id=$1`, f.bucketID)
	f.exec(t, `INSERT INTO fused_buckets(id,name) VALUES($1,$2)`, uuid.New(), buckets[0].Name)
	f.expect(t, false)
}

// TestRuntimeBucketPostgresRetainedRouting proves rerun deduplication and search preserve the selected credential scope.
func TestRuntimeBucketPostgresRetainedRouting(t *testing.T) {
	f := newRuntimeBucketDBFixture(t)
	var familyID uuid.UUID
	if err := f.base.pool.QueryRow(f.base.ctx, `SELECT app_family_id FROM fused_apps WHERE app_id=$1`, f.appID).Scan(&familyID); err != nil {
		t.Fatal(err)
	}
	source := uuid.New()
	record := ExecutionResult{ID: uuid.New(), AccountID: f.accountID, AppID: f.appID, AppFamilyID: familyID, AppTokenID: f.tokenID, AppVersion: f.base.params.Scope.Version, Status: "queued", Mode: "rerun", Input: json.RawMessage(`{}`), ReadHandleHash: strings.Repeat("a", 64), IdempotencyKeyHash: strings.Repeat("b", 64), SourceExecutionID: &source, BucketID: &f.bucketID}
	_, created, err := f.repository.CreateOrGetRerunExecutionResult(f.base.ctx, record)
	if err != nil || !created {
		t.Fatalf("reserve=%t err=%v", created, err)
	}
	assertRetainedRuntimeBucket(t, f, record)
}

// assertRetainedRuntimeBucket checks read projections and idempotency without duplicating fixture setup.
func assertRetainedRuntimeBucket(t *testing.T, f runtimeBucketDBFixture, record ExecutionResult) {
	t.Helper()
	stored, err := f.repository.GetExecutionResult(f.base.ctx, f.accountID, f.appID, record.ID)
	if err != nil || !sameRuntimeBucket(stored.BucketID, record.BucketID) {
		t.Fatalf("retained routing err=%v", err)
	}
	record.ID = uuid.New()
	_, created, err := f.repository.CreateOrGetRerunExecutionResult(f.base.ctx, record)
	if err != nil || created {
		t.Fatalf("duplicate=%t err=%v", created, err)
	}
	record.ID = uuid.New()
	replacement := uuid.New()
	record.BucketID = &replacement
	// Identical business input cannot deduplicate across different credential identities.
	if _, _, err := f.repository.CreateOrGetRerunExecutionResult(f.base.ctx, record); !errors.Is(err, ErrExecutionResultIdempotencyConflict) {
		t.Fatalf("routing conflict=%v", err)
	}
	items, err := f.repository.SearchExecutionResults(f.base.ctx, ExecutionResultSearch{AccountID: f.accountID, AppID: f.appID, Limit: 20})
	if err != nil || len(items) != 1 || items[0].BucketID == nil || *items[0].BucketID != f.bucketID {
		t.Fatalf("search count=%d err=%v", len(items), err)
	}
}

// TestRuntimeBucketPostgresCacheAndRevocation proves warmed wrappers cannot preserve revoked token or owner authority.
func TestRuntimeBucketPostgresCacheAndRevocation(t *testing.T) {
	f := newRuntimeBucketDBFixture(t)
	cached := NewCachedStore(f.repository, nil).(UnifiedAppRuntimeBucketStore)
	// Exercise the real wrapper both before and after revocation using the same instance.
	allowed, err := cached.AuthorizeUnifiedAppRuntimeBucket(f.base.ctx, f.accountID, f.appID, f.tokenID, f.bucketID)
	if err != nil || !allowed {
		t.Fatalf("initial authorization=%t err=%v", allowed, err)
	}
	f.exec(t, `UPDATE fused_app_tokens SET allow_all=false,allowed_operations=ARRAY['execute'] WHERE id=$1`, f.tokenID)
	f.expect(t, true)
	f.exec(t, `UPDATE fused_teams SET status='archived' WHERE id=$1`, f.base.params.Scope.OwnerTeamID)
	allowed, err = cached.AuthorizeUnifiedAppRuntimeBucket(f.base.ctx, f.accountID, f.appID, f.tokenID, f.bucketID)
	if err != nil || allowed {
		t.Fatalf("archived owner authorization=%t err=%v", allowed, err)
	}
	f.exec(t, `UPDATE fused_teams SET status='active' WHERE id=$1`, f.base.params.Scope.OwnerTeamID)
	f.exec(t, `DELETE FROM fused_app_tokens WHERE id=$1`, f.tokenID)
	allowed, err = cached.AuthorizeUnifiedAppRuntimeBucket(f.base.ctx, f.accountID, f.appID, f.tokenID, f.bucketID)
	if err != nil || allowed {
		t.Fatalf("revoked token authorization=%t err=%v", allowed, err)
	}
}
