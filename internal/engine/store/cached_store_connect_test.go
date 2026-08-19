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
	callbackStore, ok := cached.(CallbackConnectionStore)
	if !ok {
		t.Fatal("cached store does not expose atomic callback persistence")
	}
	connection := AuthConnection{BucketID: uuid.New(), ServiceID: uuid.New(), EndUserRef: "opaque-user"}
	resources := []ConnectionResource{{ProviderResourceID: "provider-resource"}}
	saved, returned, err := callbackStore.UpsertAuthConnectionAndReconcileResources(context.Background(), connection, resources)
	if err != nil {
		t.Fatalf("forward callback persistence: %v", err)
	}
	if delegate.calls != 1 || delegate.connection.BucketID != connection.BucketID {
		t.Fatalf("delegate call = %d connection = %#v", delegate.calls, delegate.connection)
	}
	if saved == nil || saved.ID == uuid.Nil || len(returned) != 1 || len(delegate.resources) != 1 {
		t.Fatalf("forwarded result = %#v resources = %#v", saved, returned)
	}
}

// TestCachedStoreAtomicCallbackPersistenceFailsClosed ensures unsupported
// adapters cannot silently reintroduce non-transactional callback writes.
func TestCachedStoreAtomicCallbackPersistenceFailsClosed(t *testing.T) {
	cached := NewCachedStore(&runtimeCacheDelegate{}, nil)
	callbackStore := cached.(CallbackConnectionStore)
	if _, _, err := callbackStore.UpsertAuthConnectionAndReconcileResources(context.Background(), AuthConnection{}, nil); err == nil {
		t.Fatal("expected unsupported callback persistence to fail")
	}
}
