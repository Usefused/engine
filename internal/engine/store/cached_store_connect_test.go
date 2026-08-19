package store

import (
	"context"
	"testing"

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
