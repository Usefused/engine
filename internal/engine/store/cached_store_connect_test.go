package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// callbackForwardingDelegate records the atomic callback call that must cross
// the cache wrapper in the same shape supplied by the HTTP callback handler.
type callbackForwardingDelegate struct {
	Store
	connection AuthConnection
	resources  []ConnectionResource
	calls      int
}

// refreshForwardingDelegate records every narrow refresh transition so the
// cache-wrapper regression covers both worker and foreground capabilities.
type refreshForwardingDelegate struct {
	Store
	calls      []string
	connection AuthConnection
	claim      AuthConnectionRefreshClaim
}

// ClaimAuthConnectionsForRefresh records the worker page forwarding call.
func (d *refreshForwardingDelegate) ClaimAuthConnectionsForRefresh(context.Context, time.Time, time.Time, time.Time, time.Time, int) ([]AuthConnectionRefreshClaim, error) {
	d.calls = append(d.calls, "batch")
	return []AuthConnectionRefreshClaim{d.claim}, nil
}

// TryClaimAuthConnectionRefresh records the foreground exact-claim call.
func (d *refreshForwardingDelegate) TryClaimAuthConnectionRefresh(context.Context, uuid.UUID, uuid.UUID, time.Time, time.Time) (*AuthConnectionRefreshClaim, error) {
	d.calls = append(d.calls, "exact")
	claim := d.claim
	return &claim, nil
}

// CompleteAuthConnectionRefresh records the successful lease-token CAS call.
func (d *refreshForwardingDelegate) CompleteAuthConnectionRefresh(context.Context, uuid.UUID, uuid.UUID, AuthConnection, time.Time) (*AuthConnection, bool, error) {
	d.calls = append(d.calls, "complete")
	connection := d.connection
	return &connection, true, nil
}

// ReleaseAuthConnectionRefresh records the transient retry release call.
func (d *refreshForwardingDelegate) ReleaseAuthConnectionRefresh(context.Context, uuid.UUID, uuid.UUID, time.Time, string, string, time.Time) (bool, error) {
	d.calls = append(d.calls, "release")
	return true, nil
}

// MarkAuthConnectionReconnectRequired records the permanent reconnect call.
func (d *refreshForwardingDelegate) MarkAuthConnectionReconnectRequired(context.Context, uuid.UUID, uuid.UUID, string, string, time.Time) (bool, error) {
	d.calls = append(d.calls, "reconnect")
	return true, nil
}

// GetAuthConnectionByID records the internal contention reload call.
func (d *refreshForwardingDelegate) GetAuthConnectionByID(context.Context, uuid.UUID) (*AuthConnection, error) {
	d.calls = append(d.calls, "get")
	connection := d.connection
	return &connection, nil
}

// TestCachedStoreForwardsAuthConnectionRefreshCapability guards the production
// NewCachedStore composition used by both managed and request-time refresh.
func TestCachedStoreForwardsAuthConnectionRefreshCapability(t *testing.T) {
	connectionID, versionID, leaseToken := uuid.New(), uuid.New(), uuid.New()
	delegate := &refreshForwardingDelegate{
		connection: AuthConnection{ID: connectionID, ServiceVersionID: versionID},
		claim: AuthConnectionRefreshClaim{
			Connection: AuthConnection{ID: connectionID, ServiceVersionID: versionID},
			LeaseToken: leaseToken, LeaseExpiresAt: time.Now().Add(time.Minute),
		},
	}
	cached, ok := NewCachedStore(delegate, nil).(AuthConnectionRefreshStore)
	if !ok {
		t.Fatal("cached store does not expose auth connection refresh capability")
	}
	now := time.Now().UTC()
	assertCachedRefreshClaimForwarding(t, cached, connectionID, versionID, leaseToken, now)
	assertCachedRefreshTransitionForwarding(t, cached, delegate, connectionID, leaseToken, now)
	if got := strings.Join(delegate.calls, ","); got != "batch,exact,complete,release,reconnect,get" {
		t.Fatalf("refresh forwarding calls = %q", got)
	}
}

// assertCachedRefreshClaimForwarding verifies both worker-page and foreground
// exact claims cross NewCachedStore without changing lease identity.
func assertCachedRefreshClaimForwarding(t *testing.T, cached AuthConnectionRefreshStore, connectionID, versionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	claims, err := cached.ClaimAuthConnectionsForRefresh(context.Background(), now, now, now, now.Add(time.Minute), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("forward batch claims=%#v err=%v", claims, err)
	}
	claim, err := cached.TryClaimAuthConnectionRefresh(context.Background(), connectionID, versionID, now, now.Add(time.Minute))
	if err != nil || claim == nil || claim.LeaseToken != leaseToken {
		t.Fatalf("forward exact claim=%#v err=%v", claim, err)
	}
}

// assertCachedRefreshTransitionForwarding verifies completion, transient,
// permanent, and reload calls retain the delegate's exact return contract.
func assertCachedRefreshTransitionForwarding(t *testing.T, cached AuthConnectionRefreshStore, delegate *refreshForwardingDelegate, connectionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	assertCachedRefreshCompletionForwarding(t, cached, delegate.connection, connectionID, leaseToken, now)
	assertCachedRefreshReleaseForwarding(t, cached, connectionID, leaseToken, now)
	assertCachedRefreshReconnectAndGetForwarding(t, cached, connectionID, leaseToken, now)
}

// assertCachedRefreshCompletionForwarding verifies successful CAS return values
// cross the wrapper unchanged.
func assertCachedRefreshCompletionForwarding(t *testing.T, cached AuthConnectionRefreshStore, connection AuthConnection, connectionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	completed, changed, err := cached.CompleteAuthConnectionRefresh(context.Background(), connectionID, leaseToken, connection, now)
	if err != nil || !changed || completed == nil {
		t.Fatalf("forward complete connection=%#v changed=%t err=%v", completed, changed, err)
	}
}

// assertCachedRefreshReleaseForwarding verifies transient retry state crosses
// the wrapper without a cache-side mutation.
func assertCachedRefreshReleaseForwarding(t *testing.T, cached AuthConnectionRefreshStore, connectionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	if changed, err := cached.ReleaseAuthConnectionRefresh(context.Background(), connectionID, leaseToken, now, "transient", "trace", now); err != nil || !changed {
		t.Fatalf("forward release changed=%t err=%v", changed, err)
	}
}

// assertCachedRefreshReconnectAndGetForwarding verifies permanent state and
// contention reload calls retain their exact delegate contracts.
func assertCachedRefreshReconnectAndGetForwarding(t *testing.T, cached AuthConnectionRefreshStore, connectionID, leaseToken uuid.UUID, now time.Time) {
	t.Helper()
	if changed, err := cached.MarkAuthConnectionReconnectRequired(context.Background(), connectionID, leaseToken, "invalid_grant", "trace", now); err != nil || !changed {
		t.Fatalf("forward reconnect changed=%t err=%v", changed, err)
	}
	if connection, err := cached.GetAuthConnectionByID(context.Background(), connectionID); err != nil || connection == nil || connection.ID != connectionID {
		t.Fatalf("forward get connection=%#v err=%v", connection, err)
	}
}

// UpsertAuthConnectionAndReconcileResources returns stable rows while proving
// the wrapper delegates one atomic operation rather than splitting writes.
func (d *callbackForwardingDelegate) UpsertAuthConnectionAndReconcileResources(_ context.Context, conn AuthConnection, resources []ConnectionResource) (*AuthConnection, []ConnectionResource, error) {
	d.calls++
	d.connection = conn
	d.resources = resources
	saved := conn
	saved.ID = uuid.New()
	return &saved, resources, nil
}

// TestCachedStoreForwardsAtomicCallbackPersistence guards the production store
// composition used by the Engine process, not only the raw PostgreSQL adapter.
func TestCachedStoreForwardsAtomicCallbackPersistence(t *testing.T) {
	delegate := &callbackForwardingDelegate{}
	cached := NewCachedStore(delegate, nil)
	connection := AuthConnection{BucketID: uuid.New(), ServiceID: uuid.New(), EndUserRef: "opaque-user"}
	resources := []ConnectionResource{{ProviderResourceID: "provider-resource"}}
	saved, returned, err := cached.UpsertAuthConnectionAndReconcileResources(context.Background(), connection, resources)
	// The required Store method must cross the embedded cache wrapper unchanged.
	if err != nil {
		t.Fatalf("forward callback persistence: %v", err)
	}
	// One delegate call proves the wrapper did not split or replay the transaction.
	if delegate.calls != 1 || delegate.connection.BucketID != connection.BucketID {
		t.Fatalf("delegate call = %d connection = %#v", delegate.calls, delegate.connection)
	}
	// The delegate's saved connection and authoritative resources must be returned intact.
	if saved == nil || saved.ID == uuid.Nil || len(returned) != 1 || len(delegate.resources) != 1 {
		t.Fatalf("forwarded result = %#v resources = %#v", saved, returned)
	}
}
