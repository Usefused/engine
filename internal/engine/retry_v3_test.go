package engine

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"go.opentelemetry.io/otel/trace"
)

type retryTimeoutError struct{}

func (retryTimeoutError) Error() string   { return "timeout" }
func (retryTimeoutError) Timeout() bool   { return true }
func (retryTimeoutError) Temporary() bool { return true }

func TestRetryV3FirstMatchAndWriteSafety(t *testing.T) {
	rules := retryV3Rules()
	get := &models.IntegrationObject{Method: http.MethodGet}
	capture := &retryAttemptCapture{bodyReplayable: true, requestHeaders: map[string]bool{}, responseHeaders: http.Header{}}
	if _, index, ok := firstRetryRule(rules, get, 503, capture); !ok || index != 0 {
		t.Fatalf("GET rule = %d, %v", index, ok)
	}
	post := &models.IntegrationObject{Method: http.MethodPost}
	if _, _, ok := firstRetryRule(rules, post, 503, capture); ok {
		t.Fatal("unsafe POST retried without the configured idempotency header")
	}
	capture.requestHeaders["Idempotency-Key"] = true
	if _, index, ok := firstRetryRule(rules, post, 503, capture); !ok || index != 2 {
		t.Fatalf("POST rule = %d, %v", index, ok)
	}
}

func TestRetryV3NeverRetriesOneShotBody(t *testing.T) {
	for _, readBytes := range []int{1, len("payload")} {
		t.Run(fmt.Sprintf("read_%d", readBytes), func(t *testing.T) {
			attempts := executeRetryBodyFixture(t, false, readBytes)
			if attempts != 1 {
				t.Fatalf("one-shot body attempts = %d, want 1", attempts)
			}
		})
	}
}

func TestRetryV3ReplaysIdenticalBody(t *testing.T) {
	attempts := executeRetryBodyFixture(t, true, len("payload"))
	if attempts != 2 {
		t.Fatalf("replayable body attempts = %d, want 2", attempts)
	}
}

func TestRetryV3TransportRequiresReplayableBodyAfterAnyRead(t *testing.T) {
	for _, readBytes := range []int{1, len("payload")} {
		for _, replayable := range []bool{false, true} {
			name := fmt.Sprintf("read_%d/replayable_%t", readBytes, replayable)
			t.Run(name, func(t *testing.T) {
				attempts, _ := executeRetryReplayFixture(t, context.Background(), http.MethodGet, retryFailureTransport, replayable, false, readBytes, nil)
				want := 1
				if replayable {
					want = 2
				}
				if attempts != want {
					t.Fatalf("transport attempts = %d, want %d", attempts, want)
				}
			})
		}
	}
}

func TestRetryV3UnsafeRetryRequiresKeyAndReplayableBody(t *testing.T) {
	tests := []struct {
		name       string
		failure    retryFixtureFailure
		replayable bool
		withKey    bool
		want       int
	}{
		{"status/replayable/key", retryFailureStatus, true, true, 2},
		{"status/replayable/no_key", retryFailureStatus, true, false, 1},
		{"status/one_shot/key", retryFailureStatus, false, true, 1},
		{"transport/replayable/key", retryFailureTransport, true, true, 2},
		{"transport/replayable/no_key", retryFailureTransport, true, false, 1},
		{"transport/one_shot/key", retryFailureTransport, false, true, 1},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			attempts, _ := executeRetryReplayFixture(t, context.Background(), http.MethodPost, testCase.failure, testCase.replayable, testCase.withKey, len("payload"), nil)
			if attempts != testCase.want {
				t.Fatalf("attempts = %d, want %d", attempts, testCase.want)
			}
		})
	}
}

func TestRetryV3CancellationStopsBeforeAnotherProviderAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts, err := executeRetryReplayFixture(t, ctx, http.MethodGet, retryFailureStatus, true, false, len("payload"), cancel)
	if attempts != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retry = attempts %d error %v", attempts, err)
	}
}

func TestRetryCaptureTreatsExplicitNoBodyAsReplayable(t *testing.T) {
	capture := newRetryAttemptCapture(retryV3Rules())
	ctx := context.WithValue(context.Background(), retryAttemptCaptureKey{}, capture)
	recordRetryRequest(ctx, &http.Request{Body: http.NoBody, Header: make(http.Header)})
	if !capture.bodyReplayable {
		t.Fatal("http.NoBody was classified as one-shot")
	}
}

type retryFixtureFailure string

const (
	retryFailureStatus    retryFixtureFailure = "status"
	retryFailureTransport retryFixtureFailure = "transport"
)

func executeRetryReplayFixture(t *testing.T, ctx context.Context, method string, failure retryFixtureFailure, replayable, withKey bool, readBytes int, afterAttempt func()) (int, error) {
	t.Helper()
	service := &models.Service{RetryConfig: &retrypolicy.Config{Version: retrypolicy.Version, Rules: []retrypolicy.Rule{retryReplayFixtureRule(method, failure)}}}
	operation := &models.IntegrationObject{Method: method}
	attempts := 0
	_, err := (&Dispatcher{}).executeRetryLoopV3(ctx, trace.SpanFromContext(ctx), service, operation, nil, func(attemptCtx context.Context, _ int, _ ResponseStream) (int, error) {
		request := retryBodyRequest(t, attemptCtx, replayable)
		if withKey {
			request.Header.Set("Idempotency-Key", "stable")
		}
		recordRetryRequest(attemptCtx, request)
		assertRetryFixtureRead(t, request, readBytes)
		attempts++
		if afterAttempt != nil {
			afterAttempt()
		}
		if failure == retryFailureTransport {
			err := syscall.ECONNRESET
			recordRetryTransportError(attemptCtx, err)
			return 0, err
		}
		return http.StatusServiceUnavailable, nil
	})
	return attempts, err
}

func assertRetryFixtureRead(t *testing.T, request *http.Request, readBytes int) {
	t.Helper()
	payload := make([]byte, readBytes)
	if _, err := io.ReadFull(request.Body, payload); err != nil {
		t.Fatalf("read request body: %v", err)
	}
	if string(payload) != "payload"[:readBytes] {
		t.Fatalf("attempt body = %q", payload)
	}
}

func retryReplayFixtureRule(method string, failure retryFixtureFailure) retrypolicy.Rule {
	predicates := retrypolicy.Predicates{Methods: []string{method}, BodyReplayability: retrypolicy.BodyReplayable, IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyAny}}
	if method == http.MethodPost {
		predicates.IdempotencyKey = retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyRequired, Header: "Idempotency-Key"}
	}
	if failure == retryFailureTransport {
		predicates.Errors = []retrypolicy.ErrorKind{retrypolicy.ErrorConnectionReset}
	} else {
		predicates.Statuses = []retrypolicy.StatusRange{{Min: 500, Max: 599}}
	}
	return retrypolicy.Rule{Predicates: predicates, Action: retrypolicy.Action{MaxAttempts: 2, MaxElapsedMs: 1_000, Backoff: retrypolicy.Backoff{Strategy: retrypolicy.BackoffFixed, MaxDelayMs: 1}}}
}

func executeRetryBodyFixture(t *testing.T, replayable bool, readBytes int) int {
	t.Helper()
	service := &models.Service{RetryConfig: &retrypolicy.Config{Version: retrypolicy.Version, Rules: retryV3Rules()[:1]}}
	operation := &models.IntegrationObject{Method: http.MethodGet}
	attempts := 0
	span := trace.SpanFromContext(context.Background())
	_, err := (&Dispatcher{}).executeRetryLoopV3(context.Background(), span, service, operation, nil, func(ctx context.Context, _ int, _ ResponseStream) (int, error) {
		request := retryBodyRequest(t, ctx, replayable)
		recordRetryRequest(ctx, request)
		payload := make([]byte, readBytes)
		if _, readErr := io.ReadFull(request.Body, payload); readErr != nil {
			t.Fatalf("read request body: %v", readErr)
		}
		if string(payload) != "payload"[:readBytes] {
			t.Fatalf("attempt body = %q", payload)
		}
		attempts++
		return http.StatusServiceUnavailable, nil
	})
	if err == nil {
		t.Fatal("retry fixture unexpectedly succeeded")
	}
	return attempts
}

func retryBodyRequest(t *testing.T, ctx context.Context, replayable bool) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://provider.example/items", strings.NewReader("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if !replayable {
		request.GetBody = nil
	}
	return request
}

func TestRetryErrorClassificationUsesTypedCauses(t *testing.T) {
	tests := []struct {
		err  error
		want retrypolicy.ErrorKind
	}{
		{&net.DNSError{IsTemporary: true}, retrypolicy.ErrorTemporaryDNS},
		{&net.OpError{Op: "dial", Err: retryTimeoutError{}}, retrypolicy.ErrorConnectTimeout},
		{&net.OpError{Op: "read", Err: retryTimeoutError{}}, retrypolicy.ErrorReadTimeout},
		{syscall.ECONNRESET, retrypolicy.ErrorConnectionReset},
		{x509.UnknownAuthorityError{}, retrypolicy.ErrorTLSHandshake},
		{errors.New("closed"), retrypolicy.ErrorTransport},
	}
	for _, testCase := range tests {
		if got := classifyRetryError(testCase.err); got != testCase.want {
			t.Errorf("classify %T = %s, want %s", testCase.err, got, testCase.want)
		}
	}
}

func TestQuotaBodySignalsAreBoundedObjectOnlyAndRewound(t *testing.T) {
	config := quotaBodyTestConfig()
	response := &http.Response{Body: io.NopCloser(strings.NewReader(`{"extensions":{"complexity":9}}`))}
	values, err := captureQuotaSignalBody(config, response)
	if err != nil || values["$.extensions.complexity"].(jsonNumberStringer).String() != "9" {
		t.Fatalf("capture = %#v, %v", values, err)
	}
	payload, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(payload) != `{"extensions":{"complexity":9}}` {
		t.Fatal("provider body was not rewound")
	}
	assertQuotaBodyRejected(t, config, `{"extensions":[{"complexity":9}]}`, false)
	assertQuotaBodyRejected(t, config, `{"extensions":{"complexity":9}} {}`, true)
}

type jsonNumberStringer interface{ String() string }

func assertQuotaBodyRejected(t *testing.T, config *ratelimitpolicy.Config, payload string, wantError bool) {
	t.Helper()
	response := &http.Response{Body: io.NopCloser(strings.NewReader(payload))}
	values, err := captureQuotaSignalBody(config, response)
	if wantError && err == nil {
		t.Fatal("expected body signal error")
	}
	if !wantError && (err != nil || len(values) != 0) {
		t.Fatalf("array unexpectedly matched dot-object path: %#v, %v", values, err)
	}
	if response.Body != nil {
		response.Body.Close()
	}
}

func TestRetryCaptureUsesOnlyConfiguredHeaders(t *testing.T) {
	capture := newRetryAttemptCapture(retryV3Rules())
	ctx := context.WithValue(context.Background(), retryAttemptCaptureKey{}, capture)
	response := make(http.Header)
	response.Add("Retry-After", "1")
	response.Add("Retry-After", strings.Repeat("x", maxRetrySignalHeaderBytes+1))
	response.Set("Authorization", "secret")
	recordRetryResponse(ctx, response)
	if capture.responseHeaders.Get("Authorization") != "" || capture.responseHeaders.Get("Retry-After") != "1" {
		t.Fatalf("unexpected captured headers: %#v", capture.responseHeaders)
	}
}

func quotaBodyTestConfig() *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{Name: "complexity", Mode: ratelimitpolicy.ModeObserve, Unit: ratelimitpolicy.UnitComplexity, Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityAccount}}}, Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmRollingWindow, RollingWindow: &ratelimitpolicy.RollingWindow{Limit: 100, DurationMs: 1_000}, ResponseSignals: &ratelimitpolicy.ResponseSignals{Cost: &ratelimitpolicy.ResponseSignal{Source: ratelimitpolicy.ResponseSignalBody, Path: "$.extensions.complexity"}}}}}
}

func retryV3Rules() []retrypolicy.Rule {
	action := retrypolicy.Action{MaxAttempts: 2, MaxElapsedMs: 1_000, Backoff: retrypolicy.Backoff{Strategy: retrypolicy.BackoffFixed, MaxDelayMs: 1}, RetryAfterHeaders: []retrypolicy.RetryAfterHeader{{Name: "Retry-After", Formats: []retrypolicy.RetryAfterFormat{retrypolicy.RetryAfterDeltaSeconds}, MaxDelayMs: 1_000}}}
	return []retrypolicy.Rule{
		{Predicates: retrypolicy.Predicates{Methods: []string{"GET"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationRead, retrypolicy.OperationQuery}, Statuses: []retrypolicy.StatusRange{{Min: 500, Max: 599}}, BodyReplayability: retrypolicy.BodyAny, IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyAny}}, Action: action},
		// GraphQL queries commonly use POST. The canonical policy still requires
		// a replayable body and key because transport safety is evaluated too.
		{Predicates: retrypolicy.Predicates{Methods: []string{"POST"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationQuery}, Statuses: []retrypolicy.StatusRange{{Min: 500, Max: 599}}, BodyReplayability: retrypolicy.BodyReplayable, IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyRequired, Header: "Idempotency-Key"}}, Action: action},
		{Predicates: retrypolicy.Predicates{Methods: []string{"POST"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationWrite, retrypolicy.OperationMutation}, Statuses: []retrypolicy.StatusRange{{Min: 500, Max: 599}}, BodyReplayability: retrypolicy.BodyReplayable, IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyRequired, Header: "Idempotency-Key"}}, Action: action},
	}
}

func TestProviderOperationKindClassifiesSafeMethodsAsRead(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace, "QUERY"} {
		if got := providerOperationKind(&models.IntegrationObject{Method: method}); got != retrypolicy.OperationRead {
			t.Fatalf("method %s kind=%s, want read", method, got)
		}
	}
	if got := providerOperationKind(&models.IntegrationObject{Method: "COPY"}); got != retrypolicy.OperationWrite {
		t.Fatalf("custom method kind=%s, want write", got)
	}
	mixed := &models.IntegrationObject{Method: http.MethodGet, Responses: models.Responses{"200": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream"}, {MediaType: "application/json"}}}}}
	if got := providerOperationKind(mixed); got != retrypolicy.OperationRead {
		t.Fatalf("mixed response kind=%s, want read", got)
	}
}

func TestRetryV3PublishesOnlyFinalResponseContractAndBody(t *testing.T) {
	service := &models.Service{RetryConfig: &retrypolicy.Config{Version: retrypolicy.Version, Rules: retryV3Rules()[:1]}}
	operation := &models.IntegrationObject{Method: http.MethodGet}
	stream := &mockStream{}
	status, err := (&Dispatcher{}).executeRetryLoopV3(context.Background(), trace.SpanFromContext(context.Background()), service, operation, stream, func(ctx context.Context, attempt int, attemptStream ResponseStream) (int, error) {
		recordRetryRequest(ctx, &http.Request{Method: http.MethodGet, Header: make(http.Header), Body: http.NoBody})
		if attempt == 0 {
			_ = SendResponseContract(attemptStream, http.StatusServiceUnavailable, "json")
			_ = attemptStream.Send([]byte(`{"discarded":true}`))
			return http.StatusServiceUnavailable, nil
		}
		_ = SendResponseContract(attemptStream, http.StatusCreated, "binary")
		_ = attemptStream.Send([]byte("final"))
		return http.StatusCreated, nil
	})
	if err != nil || status != http.StatusCreated {
		t.Fatalf("retry result status=%d err=%v", status, err)
	}
	if len(stream.contracts) != 1 || stream.contracts[0] != (responseContractSignal{status: http.StatusCreated, family: "binary"}) {
		t.Fatalf("published contracts=%#v", stream.contracts)
	}
	if len(stream.chunks) != 1 || string(stream.chunks[0]) != "final" || stream.bodyBeforeContract {
		t.Fatalf("published chunks=%q bodyBeforeContract=%t", stream.chunks, stream.bodyBeforeContract)
	}
}

func TestRetryElapsedDeadlineIsNotReset(t *testing.T) {
	started := time.Now()
	first := earlierDeadline(time.Time{}, started.Add(time.Second))
	second := earlierDeadline(first, started.Add(2*time.Second))
	if !second.Equal(first) {
		t.Fatal("later rule reset the logical retry deadline")
	}
}

func TestSSERetryV3ClassifiesTransportAndRetries(t *testing.T) {
	attempts := 0
	dispatcher := &Dispatcher{client: &http.Client{Transport: paginationRoundTripper(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: retry\n\n")), Request: request}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: ok\n\n")), Request: request}, nil
	})}}
	service := &models.Service{BaseURL: "https://provider.example", RetryConfig: &retrypolicy.Config{Version: retrypolicy.Version, Rules: []retrypolicy.Rule{{
		Predicates: retrypolicy.Predicates{Methods: []string{"GET"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationStream}, Statuses: []retrypolicy.StatusRange{{Min: http.StatusServiceUnavailable, Max: http.StatusServiceUnavailable}}, BodyReplayability: retrypolicy.BodyAny, IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyAny}},
		Action:     retrypolicy.Action{MaxAttempts: 2, MaxElapsedMs: 1_000, Backoff: retrypolicy.Backoff{Strategy: retrypolicy.BackoffFixed}},
	}}}}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/events", Method: http.MethodGet, Responses: models.Responses{"200": {Representations: []models.ResponseRepresentation{
		{MediaType: "application/json"}, {MediaType: "text/event-stream"},
	}}, "503": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream"}}}}})
	status, err := dispatcher.ExecuteStream(context.Background(), service, operation, nil, nil, nil, &mockStream{})
	if err != nil || status != http.StatusOK || attempts != 2 {
		t.Fatalf("SSE retry = status %d attempts %d error %v", status, attempts, err)
	}
}
