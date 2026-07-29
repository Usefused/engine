package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// fakeIdempotencyStore is an in-memory stand-in for the Postgres-backed
// idempotency cache, keyed exactly like the real table: (artifactID, keyHash).
type fakeIdempotencyStore struct {
	mu   sync.Mutex
	rows map[string]*models.IdempotentExecution
}

func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{rows: map[string]*models.IdempotentExecution{}}
}

func (f *fakeIdempotencyStore) key(artifactID uuid.UUID, keyHash string) string {
	return artifactID.String() + ":" + keyHash
}

func (f *fakeIdempotencyStore) GetIdempotentExecution(ctx context.Context, artifactID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(artifactID, idempotencyKeyHash)]
	if !ok || time.Now().After(row.ExpiresAt) {
		return nil, store.ErrIdempotentExecutionNotFound
	}
	if requestBodyHash != "" && row.RequestBodyHash != "" && row.RequestBodyHash != requestBodyHash {
		return nil, store.ErrIdempotencyKeyConflict
	}
	return row, nil
}

func (f *fakeIdempotencyStore) SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := f.key(exec.ArtifactID, exec.IdempotencyKeyHash)
	if _, exists := f.rows[k]; exists {
		return nil // first writer wins, matching the real ON CONFLICT DO NOTHING
	}
	f.rows[k] = exec
	return nil
}

// withIdempotencyStore installs s as the global idempotency store for the
// duration of the test and restores nil afterward, so this test can't leak
// state into others in the package.
func withIdempotencyStore(t *testing.T, s idempotencyStore) {
	t.Helper()
	SetIdempotencyStore(s)
	t.Cleanup(func() { SetIdempotencyStore(nil) })
}

func TestIdempotency_CacheHit_SkipsVendorCall(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true,"call":1}`))
	}))
	defer vendor.Close()

	withIdempotencyStore(t, newFakeIdempotencyStore())
	cache, endpointName := makePassthroughCache(t, vendor.URL)
	artifactID := uuid.New().String()

	ctx := contextWithExecutionIdentity(context.Background(), "idem-key-1", "body-hash-1")

	buf1 := engine.NewBufferStream()
	if err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf1); err != nil {
		t.Fatalf("first execute error: %v", err)
	}

	buf2 := engine.NewBufferStream()
	if err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf2); err != nil {
		t.Fatalf("second execute error: %v", err)
	}

	if vendorCalls != 1 {
		t.Errorf("expected exactly 1 vendor call (second should replay from cache), got %d", vendorCalls)
	}
	if buf1.String() != buf2.String() {
		t.Errorf("replayed response differs from original: got %q, want %q", buf2.String(), buf1.String())
	}
}

func TestIdempotency_DifferentKeys_BothHitVendor(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	withIdempotencyStore(t, newFakeIdempotencyStore())
	cache, endpointName := makePassthroughCache(t, vendor.URL)
	artifactID := uuid.New().String()

	ctx1 := contextWithExecutionIdentity(context.Background(), "idem-key-a", "hash-a")
	ctx2 := contextWithExecutionIdentity(context.Background(), "idem-key-b", "hash-b")

	buf := engine.NewBufferStream()
	if err := engineExecuteCore(ctx1, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("execute 1 error: %v", err)
	}
	if err := engineExecuteCore(ctx2, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("execute 2 error: %v", err)
	}

	if vendorCalls != 2 {
		t.Errorf("expected 2 vendor calls for 2 distinct idempotency keys, got %d", vendorCalls)
	}
}

func TestIdempotency_SameKeyDifferentBody_Conflicts(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()

	withIdempotencyStore(t, newFakeIdempotencyStore())
	cache, endpointName := makePassthroughCache(t, vendor.URL)
	artifactID := uuid.New().String()

	ctx1 := contextWithExecutionIdentity(context.Background(), "reused-key", "hash-original")
	ctx2 := contextWithExecutionIdentity(context.Background(), "reused-key", "hash-different")

	buf := engine.NewBufferStream()
	if err := engineExecuteCore(ctx1, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("first execute error: %v", err)
	}

	err := engineExecuteCore(ctx2, cache, engine.NewDispatcher(), &dummyTokenValidator{}, artifactID, "tok", endpointName, map[string]any{}, nil, "", buf)
	if err == nil {
		t.Fatal("expected an error when the same idempotency key is reused with a different request body")
	}
	if vendorCalls != 1 {
		t.Errorf("expected the conflicting second call to never reach the vendor, got %d vendor calls", vendorCalls)
	}
}
