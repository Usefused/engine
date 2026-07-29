package middleware

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

type runtimeStoreStub struct {
	store.Store
	versionID uuid.UUID
	err       error
}

// GetLatestWorkspaceServiceVersionID is the method RuntimeEnforcer.fetchMetadata
// actually calls (retry.go) -- it resolves the version UUID, not the
// human-readable version name, so the fetcher can hit the cached runtime
// contract snapshot. The stub previously only implemented the
// string-returning GetLatestWorkspaceServiceVersion, which nothing here
// calls anymore; that left this ID-returning method falling through to the
// embedded nil store.Store and panicking.
func (s runtimeStoreStub) GetLatestWorkspaceServiceVersionID(context.Context, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
	return s.versionID, s.err
}

type metadataFetcherStub struct {
	metadata *fusedobject.ServiceMetadata
	err      error
}

func (f metadataFetcherStub) FetchServiceMetadata(context.Context, uuid.UUID, string) (*fusedobject.ServiceMetadata, error) {
	return f.metadata, f.err
}

func TestRetryHandler_ReplaysRetryableStatus(t *testing.T) {
	attempts := 0
	forward := func(w http.ResponseWriter, r *http.Request) {
		attempts++
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"ok":true}` {
			t.Fatalf("unexpected body on attempt %d: %s", attempts, body)
		}
		if attempts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("done"))
	}

	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"ok":true}`))
	rec := httptest.NewRecorder()
	RetryHandler(rec, req, &fusedobject.RetryConfig{MaxRetries: 2}, forward)

	if attempts != 2 {
		t.Fatalf("attempts: got %d, want 2", attempts)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if rec.Body.String() != "done" {
		t.Fatalf("body: got %q, want done", rec.Body.String())
	}
}

func TestRetryHandler_DoesNotRetryNonTransientStatus(t *testing.T) {
	attempts := 0
	forward := func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}

	req := httptest.NewRequest(http.MethodGet, "/graphql", nil)
	rec := httptest.NewRecorder()
	RetryHandler(rec, req, &fusedobject.RetryConfig{MaxRetries: 2}, forward)

	if attempts != 1 {
		t.Fatalf("attempts: got %d, want 1", attempts)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestRuntimeEnforcer_UsesServiceConfig(t *testing.T) {
	globalServiceLimiter = &serviceRateLimiter{buckets: map[string]*serviceBucket{}}
	serviceID := uuid.New()
	enforcer := NewRuntimeEnforcer(
		runtimeStoreStub{versionID: uuid.New()},
		metadataFetcherStub{metadata: &fusedobject.ServiceMetadata{
			RateLimit: &fusedobject.RateLimitConfig{RequestsPerSecond: 1},
		}},
	)
	forwarded := 0
	forward := func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		w.WriteHeader(http.StatusOK)
	}

	first := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/integrations", nil)
	req.Header.Set("X-Fused-Service-ID", serviceID.String())
	enforcer.Forward(first, req, uuid.New(), forward)

	second := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/integrations", nil)
	req2.Header.Set("X-Fused-Service-ID", serviceID.String())
	enforcer.Forward(second, req2, uuid.New(), forward)

	if first.Code != http.StatusOK {
		t.Fatalf("first status: got %d, want 200", first.Code)
	}
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second status: got %d, want 429", second.Code)
	}
	if forwarded != 1 {
		t.Fatalf("forwarded: got %d, want 1", forwarded)
	}
}
