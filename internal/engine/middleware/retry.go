package middleware

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
)

type MetadataFetcher interface {
	FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error)
}

type ForwardFunc func(http.ResponseWriter, *http.Request)

type RuntimeEnforcer struct {
	store   store.Store
	fetcher MetadataFetcher
}

func NewRuntimeEnforcer(s store.Store, fetcher MetadataFetcher) *RuntimeEnforcer {
	if s == nil || fetcher == nil {
		return nil
	}
	return &RuntimeEnforcer{store: s, fetcher: fetcher}
}

func (e *RuntimeEnforcer) Forward(w http.ResponseWriter, r *http.Request, accountID uuid.UUID, forward ForwardFunc) {
	if e == nil {
		forward(w, r)
		return
	}
	serviceID, ok, err := serviceIDFromRequest(r)
	if err != nil {
		http.Error(w, `{"error":"invalid service id"}`, http.StatusBadRequest)
		return
	}
	if !ok {
		forward(w, r)
		return
	}
	metadata, err := e.fetchMetadata(r.Context(), accountID, serviceID)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadGateway)
		return
	}
	if !CheckRateLimit(serviceID.String(), metadata.RateLimit) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
		return
	}
	RetryHandler(w, r, metadata.RetryConfig, forward)
}

func (e *RuntimeEnforcer) fetchMetadata(ctx context.Context, accountID, serviceID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	// The version UUID, not the human-readable version name, is what lets
	// fetcher hit the cached runtime contract snapshot instead of always
	// making a live Registry call -- see LocalObjectCache.fetchServiceMetadata,
	// whose snapshot path only activates for a parseable version UUID.
	versionID, err := e.store.GetLatestWorkspaceServiceVersionID(ctx, accountID, serviceID)
	if err != nil {
		return nil, fmt.Errorf("service is not activated in this Engine workspace")
	}
	metadata, err := e.fetcher.FetchServiceMetadata(ctx, serviceID, versionID.String())
	if err != nil {
		return nil, fmt.Errorf("service runtime config unavailable")
	}
	return metadata, nil
}

func serviceIDFromRequest(r *http.Request) (uuid.UUID, bool, error) {
	raw := r.Header.Get("X-Fused-Service-ID")
	if raw == "" {
		raw = r.Header.Get("X-Service-ID")
	}
	if raw == "" {
		raw = r.URL.Query().Get("service_id")
	}
	if raw == "" {
		return uuid.Nil, false, nil
	}
	id, err := uuid.Parse(raw)
	return id, true, err
}

// RetryHandler wraps a forward call and replays it for configured retryable
// statuses. It buffers the response only when retry config is present.
func RetryHandler(w http.ResponseWriter, r *http.Request, config *fusedobject.RetryConfig, forward ForwardFunc) {
	if config == nil || config.MaxRetries <= 0 {
		forward(w, r)
		return
	}
	body, err := requestBodyBytes(r)
	if err != nil {
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}
	var rec *retryRecorder
	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		rec = newRetryRecorder()
		r.Body = io.NopCloser(bytes.NewReader(body))
		forward(rec, r)
		if !retryStatus(config, rec.status) {
			break
		}
		if attempt < config.MaxRetries {
			if delay := retrypolicy.BackoffDuration(config.Strategy, config.BackoffMs, attempt); delay > 0 {
				time.Sleep(delay)
			}
		}
	}
	rec.writeTo(w)
}

func requestBodyBytes(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

func retryStatus(config *fusedobject.RetryConfig, status int) bool {
	for _, code := range inferredRetryableStatuses() {
		if code == status {
			return true
		}
	}
	return false
}

func inferredRetryableStatuses() []int {
	// Provider retry config enables retries; Engine owns the conventional
	// transient HTTP failure set so Registry does not need per-service codes.
	return []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	}
}

type retryRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newRetryRecorder() *retryRecorder {
	return &retryRecorder{header: http.Header{}, status: http.StatusOK}
}

func (r *retryRecorder) Header() http.Header {
	return r.header
}

func (r *retryRecorder) Write(body []byte) (int, error) {
	return r.body.Write(body)
}

func (r *retryRecorder) WriteHeader(statusCode int) {
	r.status = statusCode
}

func (r *retryRecorder) writeTo(w http.ResponseWriter) {
	for k, vals := range r.header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(r.status)
	_, _ = w.Write(r.body.Bytes())
}
