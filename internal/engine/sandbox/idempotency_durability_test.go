package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type failingIdempotencyStore struct {
	*fakeIdempotencyStore
	readErr, saveErr error
}

// GetIdempotentExecution injects a database outage without converting it into a cache miss.
func (s *failingIdempotencyStore) GetIdempotentExecution(ctx context.Context, appID uuid.UUID, key, body string) (*models.IdempotentExecution, error) {
	// The test must distinguish unavailable state from a confirmed absent result.
	if s.readErr != nil {
		return nil, s.readErr
	}
	return s.fakeIdempotencyStore.GetIdempotentExecution(ctx, appID, key, body)
}

// SaveIdempotentExecution models a lost persistence acknowledgement after a successful provider call.
func (s *failingIdempotencyStore) SaveIdempotentExecution(ctx context.Context, record *models.IdempotentExecution) error {
	// A failed acknowledgement must leave the durable reservation pending.
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.fakeIdempotencyStore.SaveIdempotentExecution(ctx, record)
}

// TestIdempotencyLostAcknowledgement prevents a successful write from escaping before its result is durable.
func TestIdempotencyLostAcknowledgement(t *testing.T) {
	var calls atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); _, _ = w.Write([]byte(`{"created":true}`)) }))
	defer vendor.Close()
	repository := &failingIdempotencyStore{fakeIdempotencyStore: newFakeIdempotencyStore(), saveErr: errors.New("database acknowledgement lost")}
	withIdempotencyStore(t, repository)
	cache, endpoint := makeAnonymousPassthroughCache(t, vendor.URL)
	app := uuid.NewString()
	ctx := contextWithExecutionIdentity(context.Background(), "write-once", "body")
	first := engine.NewBufferStream()
	err := engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, app, "token", endpoint, map[string]any{}, nil, "", first)
	// Neither body nor success may be returned after the checkpoint fails.
	if err == nil || first.String() != "" || calls.Load() != 1 {
		t.Fatalf("err=%v body=%q calls=%d", err, first.String(), calls.Load())
	}
	repository.saveErr = nil
	err = engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, app, "token", endpoint, map[string]any{}, nil, "", engine.NewBufferStream())
	// A new attempt cannot infer that the original provider write failed.
	if !errors.Is(err, store.ErrIdempotencyInProgress) || calls.Load() != 1 {
		t.Fatalf("retry err=%v calls=%d", err, calls.Load())
	}
}

// TestIdempotencyUnavailableLookup rejects work before the provider when cached state cannot be read.
func TestIdempotencyUnavailableLookup(t *testing.T) {
	withIdempotencyStore(t, &failingIdempotencyStore{fakeIdempotencyStore: newFakeIdempotencyStore(), readErr: errors.New("offline")})
	ctx := contextWithExecutionIdentity(context.Background(), "key", "body")
	_, hit, err := lookupCachedExecution(ctx, uuid.New())
	// A storage outage must never be interpreted as permission to repeat a write.
	if hit || err == nil {
		t.Fatalf("hit=%v err=%v", hit, err)
	}
}

// TestIdempotencyConcurrentCalls reserves one owner before a blocked provider can receive a duplicate.
func TestIdempotencyConcurrentCalls(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the reservation owner may enter this handler.
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()
	withIdempotencyStore(t, newFakeIdempotencyStore())
	cache, endpoint := makeAnonymousPassthroughCache(t, vendor.URL)
	app := uuid.NewString()
	ctx := contextWithExecutionIdentity(context.Background(), "shared-key", "body")
	// Identical requests use independent streams while sharing durable key ownership.
	invoke := func() error {
		return engineExecuteCore(ctx, cache, engine.NewDispatcher(), &dummyTokenValidator{}, app, "token", endpoint, map[string]any{}, nil, "", engine.NewBufferStream())
	}
	done := make(chan error, 1)
	go func() { done <- invoke() }()
	<-started
	err := invoke()
	close(release)
	firstErr := <-done
	var rejection *IdempotencyRejectionError
	// The duplicate must fail before dispatch, then replay once the owner's result commits.
	if !errors.Is(err, store.ErrIdempotencyInProgress) || !errors.As(err, &rejection) || firstErr != nil {
		t.Fatalf("first=%v duplicate=%v", firstErr, err)
	}
	// A completed response remains usable after the concurrent reservation conflict.
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	// No duplicate write is permitted during or after the race.
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
}

// TestIdempotencyReservationRacePreservesRejection covers a competing owner winning after the initial cache miss.
func TestIdempotencyReservationRacePreservesRejection(t *testing.T) {
	withIdempotencyStore(t, newFakeIdempotencyStore())
	ctx := contextWithExecutionIdentity(context.Background(), "race-key", "same-input")
	appID := uuid.New()
	// Establish the other worker's ownership at the atomic reservation boundary.
	if _, err := reserveCachedExecution(ctx, appID); err != nil {
		t.Fatal(err)
	}
	_, err := reserveCachedExecution(ctx, appID)
	var rejection *IdempotencyRejectionError
	// The race loser needs the same safe public guidance as a pending cache lookup.
	if !errors.Is(err, store.ErrIdempotencyInProgress) || !errors.As(err, &rejection) {
		t.Fatalf("reservation race lost its pre-dispatch classification: %v", err)
	}
}

// TestIdempotencyAbsentKeyKeepsNormalExecution prevents implicit caching outside snapshot opt-in.
func TestIdempotencyAbsentKeyKeepsNormalExecution(t *testing.T) {
	var calls int
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; _, _ = w.Write([]byte(`{}`)) }))
	defer vendor.Close()
	withIdempotencyStore(t, nil)
	cache, endpoint := makeAnonymousPassthroughCache(t, vendor.URL)
	app := uuid.NewString()
	for i := 0; i < 2; i++ {
		// An ordinary invocation must remain independent even with identical parameters.
		if err := engineExecuteCore(context.Background(), cache, engine.NewDispatcher(), &dummyTokenValidator{}, app, "token", endpoint, map[string]any{}, nil, "", engine.NewBufferStream()); err != nil {
			t.Fatal(err)
		}
	}
	// Repeated unkeyed requests intentionally reach the provider twice.
	if calls != 2 {
		t.Fatalf("provider calls=%d", calls)
	}
}

// TestSnapshotProviderShapeGuard rejects operations whose effects cannot be represented by one finite response.
func TestSnapshotProviderShapeGuard(t *testing.T) {
	ctx := WithIdempotencyRun(contextWithExecutionIdentity(context.Background(), "key", "body"), uuid.New())
	for _, object := range []*models.IntegrationObject{
		{Method: "SOAP"},
		{Method: "GET", Pagination: &models.PaginationConfig{}},
		{Method: "GET", Responses: models.Responses{"200": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream"}}}}},
	} {
		// Automatic mode cannot silently skip checkpointing an unsupported operation.
		if err := validateSnapshotProvider(ctx, object); err == nil {
			t.Fatalf("unsupported shape accepted: %+v", object)
		}
	}
	// Finite writes still use the ordinary provider execution path with a durable reservation.
	if err := validateSnapshotProvider(ctx, &models.IntegrationObject{Method: "POST"}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotUnexpectedStreamDoesNotEscape guards a provider that returns streaming media absent from its contract.
func TestSnapshotUnexpectedStreamDoesNotEscape(t *testing.T) {
	output := &orderedReplayStream{}
	stream := &teeResponseStream{inner: output, cacheable: true, requireFinite: true}
	err := stream.SendResponseContract(200, "sse")
	// An unexpected stream must fail before sending any response contract or bytes to the app.
	if err == nil || len(output.events) != 0 {
		t.Fatalf("err=%v delivered=%v", err, output.events)
	}
}
