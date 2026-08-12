package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/ratelimitcoordinator"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const multiCoordinatorPageCount = 32

type coordinatorLoadEvidence struct {
	delegate             store.ProviderRateLimitStore
	acquisitions         atomic.Int64
	coordinationAttempts atomic.Int64
	localLeases          atomic.Int64
}

func (evidence *coordinatorLoadEvidence) AcquireProviderRateLimit(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	decision, err := evidence.delegate.AcquireProviderRateLimit(ctx, request)
	evidence.acquisitions.Add(1)
	evidence.coordinationAttempts.Add(decision.CoordinationAttempts)
	if decision.LocalLease {
		evidence.localLeases.Add(1)
	}
	return decision, err
}

func (evidence *coordinatorLoadEvidence) ReleaseProviderRateLimit(ctx context.Context, request ratelimitpolicy.ReleaseRequest) error {
	return evidence.delegate.ReleaseProviderRateLimit(ctx, request)
}

func (evidence *coordinatorLoadEvidence) SyncProviderRateLimit(ctx context.Context, request ratelimitpolicy.SyncRequest) error {
	return evidence.delegate.SyncProviderRateLimit(ctx, request)
}

type paginationLoadResult struct {
	status  int
	err     error
	calls   int64
	stream  *mockStream
	timings *ExecutionTimings
}

func TestPaginationQuotaCoordinatesAcrossTwoEngineInstances(t *testing.T) {
	firstCoordinator, secondCoordinator := sharedLoadTestCoordinators(t)
	firstEvidence := &coordinatorLoadEvidence{delegate: firstCoordinator}
	secondEvidence := &coordinatorLoadEvidence{delegate: secondCoordinator}
	accountID, serviceVersionID := uuid.New(), uuid.New()
	service := &models.Service{
		BaseURL:          "https://provider.test",
		ServiceVersionID: serviceVersionID,
		RateLimit:        rollingPaginationLoadPolicy(2 * multiCoordinatorPageCount),
	}

	results := make(chan paginationLoadResult, 2)
	go runPaginationLoadExecution(results, firstEvidence, service, accountID)
	go runPaginationLoadExecution(results, secondEvidence, service, accountID)
	first, second := <-results, <-results
	assertPaginationLoadResult(t, first)
	assertPaginationLoadResult(t, second)

	totalAcquisitions := firstEvidence.acquisitions.Load() + secondEvidence.acquisitions.Load()
	totalAttempts := firstEvidence.coordinationAttempts.Load() + secondEvidence.coordinationAttempts.Load()
	if totalAcquisitions != 2*multiCoordinatorPageCount {
		t.Fatalf("shared quota acquisitions = %d, want %d", totalAcquisitions, 2*multiCoordinatorPageCount)
	}
	// Rolling windows deliberately bypass local leases, so every page proves a
	// shared JetStream CAS decision instead of an Engine-local fast path.
	if totalAttempts < totalAcquisitions || totalAttempts > totalAcquisitions*256 {
		t.Fatalf("JetStream coordination attempts = %d for %d acquisitions", totalAttempts, totalAcquisitions)
	}
	if firstEvidence.localLeases.Load()+secondEvidence.localLeases.Load() != 0 {
		t.Fatal("rolling-window pagination unexpectedly used an Engine-local lease")
	}
	assertSharedQuotaExhaustedBeforeTransport(t, firstEvidence, service, accountID)
}

func runPaginationLoadExecution(results chan<- paginationLoadResult, evidence store.ProviderRateLimitStore, service *models.Service, accountID uuid.UUID) {
	var calls atomic.Int64
	stream := &mockStream{}
	timings := NewExecutionTimings()
	ctx := ContextWithExecutionTimings(context.Background(), timings)
	ctx = WithProviderRateLimitIdentity(ctx, accountID, uuid.New(), uuid.Nil)
	dispatcher := &Dispatcher{
		rateLimits: evidence,
		client:     &http.Client{Transport: paginationLoadTransport(&calls, multiCoordinatorPageCount)},
	}
	status, err := dispatcher.ExecuteStream(ctx, service, paginationLoadOperation(), nil, nil, nil, stream)
	results <- paginationLoadResult{status: status, err: err, calls: calls.Load(), stream: stream, timings: timings}
}

func paginationLoadTransport(calls *atomic.Int64, pageCount int64) http.RoundTripper {
	return paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		page := calls.Add(1) - 1
		expectedCursor := ""
		if page > 0 {
			expectedCursor = strconv.FormatInt(page, 10)
		}
		if request.URL.Query().Get("cursor") != expectedCursor {
			return nil, errors.New("pagination cursor sequence changed")
		}
		body := fmt.Sprintf(`{"items":[%d]}`, page)
		if page+1 < pageCount {
			body = fmt.Sprintf(`{"items":[%d],"next":"%d"}`, page, page+1)
		}
		return paginationResponse(request, body, nil), nil
	})
}

func paginationLoadOperation() *models.IntegrationObject {
	initial := ""
	policy := baseV3Policy("$.items")
	policy.Request = []paginationpolicy.RequestStep{{
		State: "cursor", Target: v3Target("query", "cursor"), ValueType: "string",
		Initial: v3String(initial), Apply: "all",
	}}
	policy.Response.Values = []paginationpolicy.ResponseValue{{Name: "next", Source: v3BodySource("$.next", "string")}}
	policy.Continuation = []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}}
	policy.Termination.StopOnMissingValues = []string{"next"}
	policy.Limits = paginationpolicy.Limits{
		MaxPages: multiCoordinatorPageCount, MaxItems: 2 * multiCoordinatorPageCount,
		MaxBytes: 1 << 20, MaxDurationMs: 10_000,
	}
	operation := v3Object(http.MethodGet, "/items", models.Parameter{Name: "cursor", In: "query", Type: "string"})
	operation.Pagination = modelPolicy(policy)
	return explicitAnonymousEndpoint(operation)
}

func rollingPaginationLoadPolicy(limit int64) *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{
		Name: "requests", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests,
		Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityServiceVersion}}},
		Cost:     ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmRollingWindow,
		RollingWindow: &ratelimitpolicy.RollingWindow{Limit: limit, DurationMs: 60_000},
	}}}
}

func assertPaginationLoadResult(t *testing.T, result paginationLoadResult) {
	t.Helper()
	assertPaginationLoadExecution(t, result)
	assertPaginationLoadResponse(t, result.stream)
	assertPaginationLoadSummary(t, result.timings)
}

func assertPaginationLoadExecution(t *testing.T, result paginationLoadResult) {
	t.Helper()
	if result.err != nil || result.status != http.StatusOK || result.calls != multiCoordinatorPageCount {
		t.Fatalf("pagination load status=%d err=%v calls=%d", result.status, result.err, result.calls)
	}
}

func assertPaginationLoadResponse(t *testing.T, stream *mockStream) {
	t.Helper()
	if len(stream.contracts) != 1 || len(stream.chunks) != 1 || stream.bodyBeforeContract {
		t.Fatalf("logical response contract=%#v chunks=%d body_before_contract=%t", stream.contracts, len(stream.chunks), stream.bodyBeforeContract)
	}
}

func assertPaginationLoadSummary(t *testing.T, timings *ExecutionTimings) {
	t.Helper()
	pagination := timings.PaginationSummary()
	rateLimit := timings.RateLimitSummary()
	if pagination.PageCount != multiCoordinatorPageCount || pagination.ItemCount != multiCoordinatorPageCount {
		t.Fatalf("logical pagination summary = %+v", pagination)
	}
	if timings.Count("provider_attempt_count") != multiCoordinatorPageCount {
		t.Fatalf("logical provider attempt count = %d", timings.Count("provider_attempt_count"))
	}
	if len(rateLimit.UnitTotals) != 1 || rateLimit.UnitTotals[0] != multiCoordinatorPageCount {
		t.Fatalf("logical quota summary = %+v", rateLimit)
	}
}

func assertSharedQuotaExhaustedBeforeTransport(t *testing.T, evidence store.ProviderRateLimitStore, service *models.Service, accountID uuid.UUID) {
	t.Helper()
	var calls atomic.Int64
	dispatcher := &Dispatcher{
		rateLimits: evidence,
		client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			return paginationResponse(request, `{"items":[]}`, nil), nil
		})},
	}
	ctx := WithProviderRateLimitIdentity(context.Background(), accountID, uuid.New(), uuid.Nil)
	_, err := dispatcher.ExecuteStream(ctx, service, paginationLoadOperation(), nil, nil, nil, &mockStream{})
	if !errors.Is(err, ErrProviderRateLimited) || calls.Load() != 0 {
		t.Fatalf("exhausted shared quota err=%v provider_calls=%d", err, calls.Load())
	}
}

func sharedLoadTestCoordinators(t *testing.T) (*ratelimitcoordinator.Coordinator, *ratelimitcoordinator.Coordinator) {
	t.Helper()
	natsServer, err := server.NewServer(&server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("start JetStream server: %v", err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("JetStream server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatalf("connect JetStream: %v", err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	kv, err := jetStream.CreateKeyValue(&nats.KeyValueConfig{Bucket: "TEST_PAGINATION_LOAD", History: 1})
	if err != nil {
		t.Fatalf("create quota KV: %v", err)
	}
	first, err := ratelimitcoordinator.New(kv, nil)
	if err != nil {
		t.Fatalf("create first coordinator: %v", err)
	}
	second, err := ratelimitcoordinator.New(kv, nil)
	if err != nil {
		_ = first.Close()
		t.Fatalf("create second coordinator: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})
	return first, second
}
