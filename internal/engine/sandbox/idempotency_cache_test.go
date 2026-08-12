package sandbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

type orderedReplayStream struct{ events []string }

func (stream *orderedReplayStream) Send(body []byte) error {
	stream.events = append(stream.events, "body:"+string(body))
	return nil
}

func (stream *orderedReplayStream) SendStatus(status int) error {
	stream.events = append(stream.events, fmt.Sprintf("status:%d", status))
	return nil
}

func (stream *orderedReplayStream) SendResponseContract(status int, family string) error {
	stream.events = append(stream.events, fmt.Sprintf("contract:%d:%s", status, family))
	return nil
}

// fakeIdempotencyStore is an in-memory stand-in for the Postgres-backed
// idempotency cache, keyed exactly like the real table: (appID, keyHash).
type fakeIdempotencyStore struct {
	mu   sync.Mutex
	rows map[string]*models.IdempotentExecution
}

func newFakeIdempotencyStore() *fakeIdempotencyStore {
	return &fakeIdempotencyStore{rows: map[string]*models.IdempotentExecution{}}
}

func (f *fakeIdempotencyStore) key(appID uuid.UUID, keyHash string) string {
	return appID.String() + ":" + keyHash
}

func (f *fakeIdempotencyStore) GetIdempotentExecution(ctx context.Context, appID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(appID, idempotencyKeyHash)]
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
	k := f.key(exec.AppID, exec.IdempotencyKeyHash)
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
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"call":1}`))
	}))
	defer vendor.Close()

	cacheStore := newFakeIdempotencyStore()
	withIdempotencyStore(t, cacheStore)
	cache, endpointName := makeAnonymousPassthroughCache(t, vendor.URL)
	cache.responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{{MediaType: "application/json"}}}}
	appID := uuid.New().String()

	ctx := contextWithExecutionIdentity(context.Background(), "idem-key-1", "body-hash-1")

	buf1 := engine.NewBufferStream()
	if err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf1); err != nil {
		t.Fatalf("first execute error: %v", err)
	}

	replay := &orderedReplayStream{}
	if err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", replay); err != nil {
		t.Fatalf("second execute error: %v", err)
	}

	if vendorCalls != 1 {
		t.Errorf("expected exactly 1 vendor call (second should replay from cache), got %d", vendorCalls)
	}
	wantEvents := []string{"contract:200:json", "body:" + buf1.String(), "status:200"}
	if fmt.Sprint(replay.events) != fmt.Sprint(wantEvents) {
		t.Errorf("replay order=%v, want %v", replay.events, wantEvents)
	}
}

func TestIdempotency_MixedActualSSERemainsLiveAndIsNotCached(t *testing.T) {
	var vendorCalls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: live\n\n"))
	}))
	defer vendor.Close()

	cacheStore := newFakeIdempotencyStore()
	withIdempotencyStore(t, cacheStore)
	cache, endpointName := makeAnonymousPassthroughCache(t, vendor.URL)
	cache.responses = fusedobject.Responses{"200": {Representations: []fusedobject.ResponseRepresentation{
		{MediaType: "application/json"},
		{MediaType: "text/event-stream", SSE: &fusedobject.SSEResponseContract{ItemMode: "data"}},
	}}}
	ctx := contextWithExecutionIdentity(context.Background(), "mixed-sse-key", "mixed-sse-body")
	appID := uuid.New().String()
	for call := 0; call < 2; call++ {
		if err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", engine.NewBufferStream()); err != nil {
			t.Fatalf("execute %d: %v", call+1, err)
		}
	}
	if vendorCalls != 2 || len(cacheStore.rows) != 0 {
		t.Fatalf("actual SSE calls=%d cached rows=%d, want 2 and 0", vendorCalls, len(cacheStore.rows))
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
	cache, endpointName := makeAnonymousPassthroughCache(t, vendor.URL)
	appID := uuid.New().String()

	ctx1 := contextWithExecutionIdentity(context.Background(), "idem-key-a", "hash-a")
	ctx2 := contextWithExecutionIdentity(context.Background(), "idem-key-b", "hash-b")

	buf := engine.NewBufferStream()
	if err := engineExecuteCore(ctx1, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("execute 1 error: %v", err)
	}
	if err := engineExecuteCore(ctx2, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
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
	cache, endpointName := makeAnonymousPassthroughCache(t, vendor.URL)
	appID := uuid.New().String()

	ctx1 := contextWithExecutionIdentity(context.Background(), "reused-key", "hash-original")
	ctx2 := contextWithExecutionIdentity(context.Background(), "reused-key", "hash-different")

	buf := engine.NewBufferStream()
	if err := engineExecuteCore(ctx1, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("first execute error: %v", err)
	}

	err := engineExecuteCore(ctx2, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf)
	if err == nil {
		t.Fatal("expected an error when the same idempotency key is reused with a different request body")
	}
	if vendorCalls != 1 {
		t.Errorf("expected the conflicting second call to never reach the vendor, got %d vendor calls", vendorCalls)
	}
}
