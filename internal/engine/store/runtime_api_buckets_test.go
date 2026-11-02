package store

import (
	"testing"

	"github.com/google/uuid"
)

// TestAPIRuntimeBucketPostgresAuthorization exercises the real policy for package-free API apps independently of active Unified App routing.
func TestAPIRuntimeBucketPostgresAuthorization(t *testing.T) {
	f := newRuntimeBucketDBFixture(t)
	f.exec(t, `UPDATE fused_app_families SET kind='sdk',delivery_mode='api',unified_active_app_id=NULL WHERE app_family_id=(SELECT app_family_id FROM fused_apps WHERE app_id=$1)`, f.appID)
	// An API operation is authorized separately; the bucket check must not require the Unified App's execute entry point.
	f.exec(t, `UPDATE fused_app_tokens SET allow_all=false,allowed_operations=ARRAY['listCustomers'] WHERE id=$1`, f.tokenID)
	f.expect(t, true)
	buckets, err := f.repository.ReadUnifiedAppRuntimeBuckets(f.base.ctx, f.accountID, f.appID)
	// The public projection is the same exact persisted identity used by the live decision.
	if err != nil || len(buckets) != 1 || buckets[0].ID != f.bucketID {
		t.Fatalf("API bucket scope=%v err=%v", buckets, err)
	}
	other := f
	other.bucketID = uuid.New()
	other.expect(t, false)
	f.exec(t, `UPDATE fused_app_tokens SET binding_mode='fixed' WHERE id=$1`, f.tokenID)
	// A fixed account credential cannot be redirected into another bucket by transport options.
	f.expect(t, false)
	f.exec(t, `UPDATE fused_app_tokens SET binding_mode='dynamic' WHERE id=$1`, f.tokenID)
	f.exec(t, `DELETE FROM fused_role_bindings WHERE subject_type='team' AND subject_id=$1 AND resource_id=$2`, f.base.params.Scope.OwnerTeamID, f.bucketID)
	// Removing current bucket authority invalidates even an otherwise active API app and token.
	f.expect(t, false)
}
