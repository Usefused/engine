package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAppTryConnections reads exact grant health from PostgreSQL without decrypting provider tokens.
func TestAppTryConnections(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	versionID := uuid.New()
	// Every consent grant belongs to a real immutable service version.
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, fixture.serviceID, "", "v-try", versionID, "Try Service", fixture.accountID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	_ = upsertRefreshLeaseConnection(t, fixture, "healthy", versionID, now.Add(time.Hour))
	_ = upsertRefreshLeaseConnection(t, fixture, "refreshable", versionID, now.Add(-time.Minute))
	_ = upsertRefreshLeaseConnectionState(t, fixture, "revoked", versionID, now.Add(time.Hour), "reconnect_required")
	expired := upsertRefreshLeaseConnection(t, fixture, "expired", versionID, now.Add(-time.Minute))
	// Remove only this fixture's refresh token to exercise truly unusable expired access.
	if _, err := fixture.store.(*postgresStore).db.Exec(fixture.ctx, "UPDATE fused_auth_connections SET refresh_token=NULL WHERE id=$1", expired.ID); err != nil {
		t.Fatal(err)
	}
	requests := []AppTryConnectionRequest{}
	for _, user := range []string{"healthy", "refreshable", "revoked", "expired", "missing", ""} {
		requests = append(requests, AppTryConnectionRequest{BucketID: fixture.bucketA, ServiceID: fixture.serviceID, AuthName: "oauth", AuthType: "oauth", EndUserRef: user})
	}
	// Same user and service in another bucket must never reuse this grant.
	other := requests[0]
	other.BucketID = fixture.bucketB
	requests = append(requests, other)
	result, err := fixture.store.(AppTryConnectionStore).ReadAppTryConnections(fixture.ctx, requests)
	want := []string{"ready", "ready", "reconnect_required", "reconnect_required", "missing", "user_required", "missing"}
	if err != nil || !reflect.DeepEqual(result, want) {
		t.Fatalf("states=%v want=%v error=%v", result, want, err)
	}
}
