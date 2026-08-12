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
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type providerRateLimitStoreStub struct {
	mu        sync.Mutex
	delay     time.Duration
	decisions []ratelimitpolicy.Decision
	requests  []ratelimitpolicy.AcquireRequest
	syncs     []ratelimitpolicy.SyncRequest
	releases  []ratelimitpolicy.ReleaseRequest
}

func (s *providerRateLimitStoreStub) ReleaseProviderRateLimit(_ context.Context, request ratelimitpolicy.ReleaseRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, request)
	return nil
}

func (s *providerRateLimitStoreStub) AcquireProviderRateLimit(_ context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	time.Sleep(s.delay)
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
	if request.Policies[0].ScopeKind != string(ratelimitpolicy.IdentityConnection) || request.Policies[0].ScopeID == "" {
		t.Fatalf("connection identity was not resolved safely: %#v", request.Policies[0])
	}
	if request.Policies[1].ScopeKind != string(ratelimitpolicy.IdentityServiceVersion) || request.Policies[1].ScopeID == "" {
		t.Fatalf("service-version identity was not resolved safely: %#v", request.Policies[1])
	}

	ctx = WithProviderRateLimitIdentity(context.Background(), accountID, bucketID, uuid.Nil)
	if _, err = providerRateLimitRequest(ctx, srv, &models.IntegrationObject{StableKey: "operation-name-is-not-a-key"}); err == nil {
		t.Fatal("missing canonical connection identity did not fail closed")
	}
}

func TestProviderConcurrencyRequiresExecutionDeadline(t *testing.T) {
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.New())
	service := &models.Service{ServiceVersionID: uuid.New(), RateLimit: &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{
		Name: "parallel", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests,
		Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityConnection}}}, Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmConcurrency,
		Concurrency: &ratelimitpolicy.Concurrency{Limit: 1},
	}}}}
	if _, err := providerRateLimitRequest(ctx, service, &models.IntegrationObject{StableKey: "rest:GET:/items"}); err == nil {
		t.Fatal("expected missing concurrency deadline to fail closed")
	}
}

func TestProviderRateLimitDenialDoesNotSleep(t *testing.T) {
	store := &providerRateLimitStoreStub{decisions: []ratelimitpolicy.Decision{{Allowed: false, RetryAfter: time.Hour}}}
	dispatcher := NewDispatcherWithProviderRateLimits(store)
	srv := &models.Service{ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	started := time.Now()
	_, err := dispatcher.awaitProviderRateLimit(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/items"})
	if err != ErrProviderRateLimited {
		t.Fatalf("error = %v, want local denial", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("non-blocking quota denial took %s", elapsed)
	}
	if len(store.requests) != 1 {
		t.Fatalf("acquisitions = %d, want exactly one", len(store.requests))
	}
}

func TestProviderRateLimitDenialDoesNotReacquire(t *testing.T) {
	store := &providerRateLimitStoreStub{decisions: []ratelimitpolicy.Decision{
		{Allowed: false, RetryAfter: time.Hour}, {Allowed: false, RetryAfter: time.Hour},
	}}
	dispatcher := NewDispatcherWithProviderRateLimits(store)
	srv := &models.Service{ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	started := time.Now()
	_, err := dispatcher.awaitProviderRateLimit(ctx, srv, &models.IntegrationObject{StableKey: "rest:GET:/items"})
	if err != ErrProviderRateLimited {
		t.Fatalf("error = %v, want local denial", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("wait %s exceeded the total max-delay budget", elapsed)
	}
	if len(store.requests) != 1 {
		t.Fatalf("acquisitions = %d, want no Engine-side quota retry", len(store.requests))
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
		RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()},
	}
	ctx := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.Nil)
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(&models.IntegrationObject{
		Path: "/items", Method: http.MethodGet, StableKey: "rest:GET:/items",
	}), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if attempts != 2 || len(rateStore.requests) != 2 || len(rateStore.syncs) != 0 {
		t.Fatalf("attempts=%d acquires=%d syncs=%d, want two charges and no headerless sync writes", attempts, len(rateStore.requests), len(rateStore.syncs))
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
		Path: "/events", Method: http.MethodGet, StableKey: "rest:GET:/events",
	}), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || len(rateStore.requests) != 1 {
		t.Fatalf("status=%d err=%v acquisitions=%d", status, err, len(rateStore.requests))
	}
}

func TestEachPaginationPageAcquiresAndAccumulatesTimings(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	calls := 0
	dispatcher := &Dispatcher{rateLimits: rateStore, client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		body := `{"values":[1],"next":"/items?page=2"}`
		if calls == 2 {
			body = `{"values":[2]}`
		}
		return paginationResponse(request, body, nil), nil
	})}}
	caseData := v3HybridCase()
	policy := modelPolicy(caseData.policy)
	srv := &models.Service{BaseURL: "https://provider.test", ServiceVersionID: uuid.New(), RateLimit: fixedRateLimitFixture(5)}
	timings := NewExecutionTimings()
	ctx := ContextWithExecutionTimings(context.Background(), timings)
	ctx = WithProviderRateLimitIdentity(ctx, uuid.New(), uuid.New(), uuid.Nil)
	caseData.object.Pagination = policy
	caseData.object.StableKey = "rest:GET:/items"
	status, err := dispatcher.ExecuteStream(ctx, srv, explicitAnonymousEndpoint(caseData.object), nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || calls != 2 || len(rateStore.requests) != 2 {
		t.Fatalf("status=%d err=%v calls=%d acquisitions=%d", status, err, calls, len(rateStore.requests))
	}
	snapshot := timings.SnapshotMilliseconds()
	_, hasProviderTotal := snapshot["provider_total"]
	_, hasRateLimitAcquire := snapshot["rate_limit_acquire_ms"]
	// Exact elapsed time belongs in the opt-in benchmark; the checked gate uses
	// page/acquisition counts and only requires both timing dimensions to exist.
	if !hasProviderTotal || !hasRateLimitAcquire {
		t.Fatalf("pagination timing dimensions are incomplete: %#v", snapshot)
	}
}

func TestProviderHeaderObservationAnd429CooldownAreBounded(t *testing.T) {
	rateStore := &providerRateLimitStoreStub{}
	dispatcher := NewDispatcherWithProviderRateLimits(rateStore)
	config := fixedRateLimitFixture(5)
	config.Policies[0].ResponseSignals = &ratelimitpolicy.ResponseSignals{
		Limit:     &ratelimitpolicy.ResponseSignal{Source: ratelimitpolicy.ResponseSignalHeader, Name: "X-Limit"},
		Remaining: &ratelimitpolicy.ResponseSignal{Source: ratelimitpolicy.ResponseSignalHeader, Name: "X-Remaining"},
		Reset: &ratelimitpolicy.ResetSignal{
			Signal: ratelimitpolicy.ResponseSignal{Source: ratelimitpolicy.ResponseSignalHeader, Name: "X-Reset"},
			Format: ratelimitpolicy.ResetDeltaSeconds,
		},
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
	dispatcher := NewDispatcherWithProviderRateLimits(&providerRateLimitStoreStub{decisions: []ratelimitpolicy.Decision{{Allowed: true}}})
	timings := NewExecutionTimings()
	ctx := ContextWithExecutionTimings(context.Background(), timings)
	_, err := dispatcher.acquireProviderRateLimit(ctx, ratelimitpolicy.AcquireRequest{Policies: []ratelimitpolicy.ResolvedPolicy{
		{Unit: "points", Cost: 10, ScopeKind: "connection"},
		{Unit: "requests", Cost: 1, ScopeKind: "service_version"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(spans))
	}
	if _, ok := timings.SnapshotMilliseconds()["rate_limit_acquire_ms"]; !ok {
		t.Fatal("rate-limit acquisition timing was not recorded")
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
	return &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{
		Name: "requests", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests,
		Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityServiceVersion}}},
		Cost:     ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmFixedWindow,
		FixedWindow: &ratelimitpolicy.FixedWindow{Limit: limit, DurationMs: 60_000},
	}}, Cooldown: &ratelimitpolicy.Cooldown{
		Statuses: []ratelimitpolicy.StatusRange{{Min: http.StatusTooManyRequests, Max: http.StatusTooManyRequests}},
		Headers:  []ratelimitpolicy.CooldownHeader{{Name: "Retry-After", Formats: []ratelimitpolicy.ResetFormat{ratelimitpolicy.ResetDeltaSeconds}, MaxDelayMs: 1_000}},
	}}
}

func mixedRateLimitFixture() *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{
		{Name: "service_requests", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests, Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityServiceVersion}}}, Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmFixedWindow, FixedWindow: &ratelimitpolicy.FixedWindow{Limit: 100, DurationMs: 60_000}},
		{Name: "connection_points", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitPoints, Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityConnection}}}, Cost: ratelimitpolicy.CostPlan{Default: 1, Rules: []ratelimitpolicy.CostRule{{Operation: "rest:GET:/drive/v3/files/{}", Cost: 10}}}, Algorithm: ratelimitpolicy.AlgorithmTokenBucket, TokenBucket: &ratelimitpolicy.TokenBucket{Capacity: 100, RefillUnits: 10, RefillIntervalMs: 1_000}},
	}}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
