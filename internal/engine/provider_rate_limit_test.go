package engine

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type providerRateLimitStoreStub struct {
	mu        sync.Mutex
	decisions []ratelimitpolicy.Decision
	requests  []ratelimitpolicy.AcquireRequest
	syncs     []ratelimitpolicy.SyncRequest
}

func (s *providerRateLimitStoreStub) AcquireProviderRateLimit(_ context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	if len(s.decisions) == 0 {
		return ratelimitpolicy.Decision{Allowed: true}, nil
	}
	decision := s.decisions[0]
	s.decisions = s.decisions[1:]
	return decision, nil
}

func (s *providerRateLimitStoreStub) SyncProviderRateLimit(_ context.Context, request ratelimitpolicy.SyncRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncs = append(s.syncs, request)
	return nil
}

func TestProviderRateLimitRequestUsesStableKeyAndExactScopes(t *testing.T) {
	accountID, versionID, bucketID, connectionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	ctx := WithProviderRateLimitIdentity(context.Background(), accountID, bucketID, connectionID)
	srv := &models.Service{ServiceVersionID: versionID, RateLimit: mixedRateLimitFixture()}
	request, err := providerRateLimitRequest(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/drive/v3/files/{}"})
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Policies) != 2 || request.Policies[0].Name != "connection_points" || request.Policies[0].Cost != 10 {
		t.Fatalf("unexpected resolved policies: %#v", request.Policies)
	}
	if request.Policies[0].ScopeID != connectionID.String() {
		t.Fatalf("connection scope = %s, want exact auth connection %s", request.Policies[0].ScopeID, connectionID)
	}
	if request.Policies[1].ScopeID != versionID.String() {
		t.Fatalf("service scope = %s, want version %s", request.Policies[1].ScopeID, versionID)
	}

	ctx = WithProviderRateLimitIdentity(context.Background(), accountID, bucketID, uuid.Nil)
	request, err = providerRateLimitRequest(ctx, srv, &models.IntegrationObject{StableKey: "operation-name-is-not-a-key"})
	if err != nil {
		t.Fatal(err)
	}
	if request.Policies[0].ScopeID != bucketID.String() || request.Policies[0].Cost != 1 {
		t.Fatalf("bucket fallback/default cost failed: %#v", request.Policies[0])
	}
}

func TestProviderRateLimitWaitIsCappedAndContextAware(t *testing.T) {
	store := &providerRateLimitStoreStub{decisions: []ratelimitpolicy.Decision{{Allowed: false, RetryAfter: time.Hour}}}
	dispatcher := NewDispatcherWithProviderRateLimits(store)
	srv := &models.Service{ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	ctx, cancel := context.WithCancel(WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil))
	cancel()
	_, err := dispatcher.awaitProviderRateLimit(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/items"})
	if err != context.Canceled {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if len(store.requests) != 1 {
		t.Fatalf("acquisitions = %d, want one before cancellation", len(store.requests))
	}
}

func TestProviderRateLimitMaximumDelayIsTotalBudget(t *testing.T) {
	store := &providerRateLimitStoreStub{decisions: []ratelimitpolicy.Decision{
		{Allowed: false, RetryAfter: time.Hour}, {Allowed: false, RetryAfter: time.Hour},
	}}
	dispatcher := NewDispatcherWithProviderRateLimits(store)
	config := fixedRateLimitFixture(5)
	config.RetryAfter.MaxDelayMS = 5
	srv := &models.Service{ServiceVersionID: uuid.New(), RateLimit: config}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	started := time.Now()
	_, err := dispatcher.awaitProviderRateLimit(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/items"})
	if err != ErrProviderRateLimited {
		t.Fatalf("error = %v, want local denial", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("wait %s exceeded the total max-delay budget", elapsed)
	}
	if len(store.requests) != 2 {
		t.Fatalf("acquisitions = %d, want one recheck after capped wait", len(store.requests))
	}
}

func TestEveryRetryAttemptAcquiresImmediatelyBeforeTransport(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	attempts := 0
	dispatcher := &Dispatcher{
		rateLimits: rateStore,
		client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			status := http.StatusServiceUnavailable
			if attempts == 2 {
				status = http.StatusOK
			}
			return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
		})},
	}
	srv := &models.Service{
		BaseURL: "https://provider.test", ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5),
		RetryConfig: &models.RetryConfig{Strategy: "fixed", MaxRetries: 1},
	}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(&models.IntegrationObject{
		Path: "/items", Method: http.MethodGet, StableKey: "rest:GET:/items",
	}), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if attempts != 2 || len(rateStore.requests) != 2 || len(rateStore.syncs) != 2 {
		t.Fatalf("attempts=%d acquires=%d syncs=%d, want 2 each", attempts, len(rateStore.requests), len(rateStore.syncs))
	}
}

func TestSSEAttemptAcquiresBeforeTransport(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	dispatcher := &Dispatcher{rateLimits: rateStore, client: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data: {}\n\n")), Request: request}, nil
	})}}
	srv := &models.Service{BaseURL: "https://provider.test", ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(&models.IntegrationObject{
		Path: "/events", Method: http.MethodGet, IsSSE: true, StableKey: "rest:GET:/events",
	}), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || len(rateStore.requests) != 1 {
		t.Fatalf("status=%d err=%v acquisitions=%d", status, err, len(rateStore.requests))
	}
}

func TestEachPaginationPageAcquires(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	calls := 0
	dispatcher := &Dispatcher{rateLimits: rateStore, client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		body := `{"items":[1],"next":"/items?page=2"}`
		if calls == 2 {
			body = `{"items":[2]}`
		}
		return paginationResponse(request, body, nil), nil
	})}}
	policy := modelPolicy(paginationpolicy.Config{
		Version: 2, Type: "next_url", ItemsPath: "$.items",
		NextURL: &paginationpolicy.NextURLConfig{Next: paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "url"}},
	})
	srv := &models.Service{BaseURL: "https://provider.test", ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(&models.IntegrationObject{
		Path: "/items", Method: http.MethodGet, Pagination: policy, StableKey: "rest:GET:/items",
	}), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || calls != 2 || len(rateStore.requests) != 2 {
		t.Fatalf("status=%d err=%v calls=%d acquisitions=%d", status, err, calls, len(rateStore.requests))
	}
}

func TestProviderHeaderObservationAnd429CooldownAreBounded(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	dispatcher := NewDispatcherWithProviderRateLimits(rateStore)
	config := fixedRateLimitFixture(5)
	config.Policies[0].ResponseHeaders = &ratelimitpolicy.ResponseHeaders{
		Limit: "X-Limit", Remaining: "X-Remaining",
		Reset: &ratelimitpolicy.ResetHeader{Name: "X-Reset", Format: "delta_seconds"},
	}
	srv := &models.Service{ServiceVersionID: uuid.New(), RateLimit: config}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{
		"X-Limit": {"3"}, "X-Remaining": {"0"}, "X-Reset": {"30"}, "Retry-After": {"3600"},
	}}
	if err := dispatcher.syncProviderRateLimitResponse(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/items"}, response); err != nil {
		t.Fatal(err)
	}
	if len(rateStore.syncs) != 1 || len(rateStore.syncs[0].Observations) != 1 {
		t.Fatalf("sync = %#v", rateStore.syncs)
	}
	assertProviderHeaderObservation(t, rateStore.syncs[0].Observations[0])
	delay := time.Until(*rateStore.syncs[0].CooldownUntil)
	if delay <= 0 || delay > 1100*time.Millisecond {
		t.Fatalf("cooldown delay %s was not capped near one second", delay)
	}
}

func assertProviderHeaderObservation(t *testing.T, observation ratelimitpolicy.ResponseObservation) {
	t.Helper()
	if observation.Limit == nil || *observation.Limit != 3 {
		t.Fatalf("limit observation = %#v", observation.Limit)
	}
	if observation.Remaining == nil || *observation.Remaining != 0 {
		t.Fatalf("remaining observation = %#v", observation.Remaining)
	}
	if observation.ResetAt == nil {
		t.Fatal("reset observation is missing")
	}
}

func TestProviderRateLimitTelemetryHasOnlySafeAggregateKeys(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previous)
	})
	recordProviderRateLimitDecision(context.Background(), ratelimitpolicy.Decision{
		Allowed: true, PolicyCount: 2, ScopeKinds: []string{"connection"},
		UnitTotals: map[string]int64{"points": 10},
	}, nil)
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	for _, attribute := range spans[0].Attributes() {
		key := string(attribute.Key)
		for _, forbidden := range []string{"policy_name", "scope_id", "config_hash", "header_name", "header_value"} {
			if strings.Contains(key, forbidden) {
				t.Fatalf("unsafe rate-limit telemetry key %q", key)
			}
		}
	}
}

func fixedRateLimitFixture(limit int64) *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: 2, Policies: []ratelimitpolicy.Policy{{
		Name: "requests", Unit: "requests", Scope: "service_version", DefaultCost: 1,
		OperationCosts: map[string]int64{}, Algorithm: "fixed_window",
		FixedWindow: &ratelimitpolicy.FixedWindow{Limit: limit, DurationMS: 60_000},
	}}, RetryAfter: &ratelimitpolicy.RetryAfter{Enabled: true, MaxDelayMS: 1_000}}
}

func mixedRateLimitFixture() *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: 2, Policies: []ratelimitpolicy.Policy{
		{Name: "service_requests", Unit: "requests", Scope: "service_version", DefaultCost: 1, OperationCosts: map[string]int64{}, Algorithm: "fixed_window", FixedWindow: &ratelimitpolicy.FixedWindow{Limit: 100, DurationMS: 60_000}},
		{Name: "connection_points", Unit: "points", Scope: "connection", DefaultCost: 1, OperationCosts: map[string]int64{"rest:GET:/drive/v3/files/{}": 10}, Algorithm: "token_bucket", TokenBucket: &ratelimitpolicy.TokenBucket{Capacity: 100, RefillUnits: 10, RefillIntervalMS: 1_000}},
	}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
