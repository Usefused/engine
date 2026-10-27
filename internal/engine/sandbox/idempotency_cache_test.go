package sandbox

import (
	"context"
	"errors"
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

// GetIdempotentExecution models the persistent conflict and pending-outcome gates.
func (f *fakeIdempotencyStore) GetIdempotentExecution(ctx context.Context, appID uuid.UUID, keyHash, bodyHash string) (*models.IdempotentExecution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[f.key(appID, keyHash)]
	// Only completed standalone rows expire by their TTL.
	if !ok || (!row.Pending && row.RunID == nil && time.Now().After(row.ExpiresAt)) {
		return nil, store.ErrIdempotentExecutionNotFound
	}
	// Changing inputs cannot borrow an existing result.
	if row.RequestBodyHash != bodyHash {
		return nil, store.ErrIdempotencyKeyConflict
	}
	// Pending outcomes cannot authorize a second dispatch.
	if row.Pending {
		return nil, store.ErrIdempotencyInProgress
	}
	copy := *row
	return &copy, nil
}

// ReserveIdempotentExecution gives exactly one concurrent caller ownership of a key.
func (f *fakeIdempotencyStore) ReserveIdempotentExecution(ctx context.Context, execution *models.IdempotentExecution) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(execution.AppID, execution.IdempotencyKeyHash)
	// The fake shares PostgreSQL's unique-key fence.
	if _, ok := f.rows[key]; ok {
		return store.ErrIdempotencyInProgress
	}
	copy := *execution
	copy.Pending = true
	f.rows[key] = &copy
	return nil
}

// SaveIdempotentExecution completes only the caller's existing reservation.
func (f *fakeIdempotencyStore) SaveIdempotentExecution(ctx context.Context, execution *models.IdempotentExecution) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := f.key(execution.AppID, execution.IdempotencyKeyHash)
	previous := f.rows[key]
	// Persistence cannot acknowledge a worker that lost ownership.
	if previous == nil || previous.ID != execution.ID || !previous.Pending {
		return store.ErrIdempotencyInProgress
	}
	copy := *execution
	copy.Pending = false
	f.rows[key] = &copy
	return nil
}

// ReleaseIdempotentExecution removes only the owned pending record after repeatable work.
func (f *fakeIdempotencyStore) ReleaseIdempotentExecution(ctx context.Context, id uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, row := range f.rows {
		// Other keys and completed responses must survive a failed read.
		if row.ID == id && row.Pending {
			delete(f.rows, key)
		}
	}
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

// TestIdempotency_SameKeyDifferentBody_Conflicts preserves the typed rejection through actual dispatch.
func TestIdempotency_SameKeyDifferentBody_Conflicts(t *testing.T) {
	var vendorCalls int
	// Count real provider calls independently of the cache's claimed outcome.
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
	// The original request must commit a replayable response before testing conflict reporting.
	if err := engineExecuteCore(ctx1, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf); err != nil {
		t.Fatalf("first execute error: %v", err)
	}

	err := engineExecuteCore(ctx2, cache, engine.NewDispatcher(), &dummyTokenValidator{}, appID, "tok", endpointName, map[string]any{}, nil, "", buf)
	var rejection *IdempotencyRejectionError
	// Transports need both the stable cause and proof that this attempt never dispatched.
	if !errors.Is(err, store.ErrIdempotencyKeyConflict) || !errors.As(err, &rejection) {
		t.Fatalf("expected a typed pre-dispatch key conflict, got %v", err)
	}
	// Improving the error must not weaken the duplicate-write fence.
	if vendorCalls != 1 {
		t.Errorf("expected the conflicting second call to never reach the vendor, got %d vendor calls", vendorCalls)
	}
}

// RetainIdempotentExecution leaves run lifetime to PostgreSQL tests while satisfying the shared cache contract.
func (f *fakeIdempotencyStore) RetainIdempotentExecution(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
