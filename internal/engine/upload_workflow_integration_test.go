package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestUploadWorkflowUsesCanonicalAttemptControls(t *testing.T) {
	attempts := 0
	var vendor *httptest.Server
	vendor = httptest.NewServer(uploadVendorHandler(t, &attempts, func() string { return vendor.URL }))
	defer vendor.Close()

	rateStore := &providerRateLimitStoreStub{}
	dispatcher := NewDispatcherWithProviderRateLimits(rateStore)
	workflow := tinyDispatcherUploadWorkflow()
	service := &models.Service{BaseURL: vendor.URL, ServiceVersionID: uuid.New(),
		AuthConfigs:    models.AuthConfigs{{Name: "bearer", Type: "http", Scheme: "bearer"}},
		DefaultHeaders: models.DefaultHeaders{"Idempotency-Key": "stable"},
		RetryConfig:    &models.RetryConfig{Version: retrypolicy.Version, Rules: uploadRetryRules()}, RateLimit: uploadConcurrencyPolicy(),
	}
	operation := &models.IntegrationObject{StableKey: "rest:POST:/upload", Method: http.MethodPost,
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "bearer"}}}},
		RequestContent:       uploadRequestContent(workflow),
	}
	timings := NewExecutionTimings()
	base := ContextWithExecutionTimings(context.Background(), timings)
	base = ContextWithIdempotencyKeyPresent(base, true)
	ctx, cancel := context.WithTimeout(WithProviderRateLimitIdentity(base, uuid.New(), uuid.New(), uuid.New()), time.Second)
	defer cancel()
	status, err := dispatcher.ExecuteStream(ctx, service, operation, map[string]any{
		"upload_mode": "resumable", "media": []byte("12345678"), "chunk_size_bytes": 4,
	}, map[string]any{"bearer": "token"}, nil, NewBufferStream())
	if err != nil || status != http.StatusCreated {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if attempts != 5 || len(rateStore.requests) != 5 || len(rateStore.releases) != 5 {
		t.Fatalf("attempts=%d acquires=%d releases=%d", attempts, len(rateStore.requests), len(rateStore.releases))
	}
	_, hasProviderTiming := timings.SnapshotMilliseconds()["provider_total"]
	if timings.Count("provider_attempt_count") != 5 || !hasProviderTiming {
		t.Fatalf("timing/count missing: count=%d timings=%v", timings.Count("provider_attempt_count"), timings.SnapshotMilliseconds())
	}
}

func TestUploadWorkflowV3DoesNotRetryStreamingMultipartBody(t *testing.T) {
	attempts := 0
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer vendor.Close()
	workflow := &workflowcontract.UploadWorkflow{Version: 1, AcceptedMediaTypes: []string{"application/octet-stream"}, Modes: []workflowcontract.UploadMode{{
		Kind: workflowcontract.UploadMultipart, Steps: []workflowcontract.UploadStep{{Kind: workflowcontract.StepTransfer, Method: "POST", URL: workflowcontract.URLSource{Kind: workflowcontract.URLDeclaredPath, Path: "/multipart"}, Body: workflowcontract.BodyMultipart, SuccessStatuses: []workflowcontract.StatusRange{{Min: 200, Max: 299}}}},
	}}}
	service := &models.Service{BaseURL: vendor.URL, RetryConfig: &models.RetryConfig{Version: retrypolicy.Version, Rules: retryV3Rules()}, DefaultHeaders: models.DefaultHeaders{"Idempotency-Key": "stable"}}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Method: "POST", RequestContent: uploadRequestContent(workflow)})
	_, err := NewDispatcher().ExecuteStream(context.Background(), service, operation, map[string]any{"upload_mode": "multipart", "media": []byte("payload")}, nil, nil, NewBufferStream())
	if err == nil || attempts != 1 {
		t.Fatalf("multipart err=%v attempts=%d", err, attempts)
	}
}

func TestUploadWorkflowHoldsConcurrencyPermitUntilResponseBodyCloses(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"ok":true}`))
	}))
	defer provider.Close()
	rateStore := &providerRateLimitStoreStub{}
	dispatcher := NewDispatcherWithProviderRateLimits(rateStore)
	service := &models.Service{BaseURL: provider.URL, ServiceVersionID: uuid.New(), RateLimit: uploadConcurrencyPolicy()}
	operation := uploadResponseOperation(models.Responses{"200": {Representations: []models.ResponseRepresentation{{MediaType: "application/json"}}}})
	stream := &releaseInspectingStream{store: rateStore, inner: &mockStream{}, expectedReleasesDuringSend: 0}
	identityContext := WithProviderRateLimitIdentity(context.Background(), uuid.New(), uuid.New(), uuid.New())
	ctx, cancel := context.WithTimeout(identityContext, time.Second)
	defer cancel()
	status, err := dispatcher.ExecuteStream(ctx, service, operation, map[string]any{
		"upload_mode": "multipart", "media": []byte("payload"),
	}, nil, nil, stream)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if stream.releaseObservedTooEarly || len(rateStore.releases) != 1 {
		t.Fatalf("early=%t releases=%d", stream.releaseObservedTooEarly, len(rateStore.releases))
	}
}

type releaseInspectingStream struct {
	store                      *providerRateLimitStoreStub
	inner                      *mockStream
	expectedReleasesDuringSend int
	releaseObservedTooEarly    bool
}

func (stream *releaseInspectingStream) Send(chunk []byte) error {
	stream.releaseObservedTooEarly = len(stream.store.releases) != stream.expectedReleasesDuringSend
	return stream.inner.Send(chunk)
}

func (stream *releaseInspectingStream) SendResponseContract(status int, family string) error {
	return stream.inner.SendResponseContract(status, family)
}

func TestUploadWorkflowKeepsUndeclaredSSEResponseOpaque(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = response.Write([]byte("data: tenant-secret\n\n"))
	}))
	defer provider.Close()
	stream := &mockStream{}
	operation := uploadResponseOperation(models.Responses{"200": {Representations: []models.ResponseRepresentation{{MediaType: "application/json"}}}})
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation, map[string]any{
		"upload_mode": "multipart", "media": []byte("payload"),
	}, nil, nil, stream)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if len(stream.contracts) != 1 || stream.contracts[0].family != "unknown" {
		t.Fatalf("response contract = %#v", stream.contracts)
	}
	if got := string(bytes.Join(stream.chunks, nil)); got != "data: tenant-secret\n\n" {
		t.Fatalf("opaque response = %q", got)
	}
}

func TestUploadWorkflowSelectsResponseContractFromActualResponse(t *testing.T) {
	done := "[END]"
	responses := models.Responses{
		"200": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream", SSE: &models.SSEResponseContract{ItemMode: "data", DoneSentinel: &done}}}},
		"201": {Representations: []models.ResponseRepresentation{{MediaType: "application/vnd.acme+json"}}},
		"206": {Representations: []models.ResponseRepresentation{{MediaType: "application/octet-stream"}}},
	}
	binaryBody := bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 2000)
	provider := httptest.NewServer(mixedResponseProvider(t, binaryBody))
	defer provider.Close()

	tests := []mixedResponseCase{
		{variant: "sse", status: 200, family: "sse", body: []byte("one\ntwo"), chunks: 1},
		{variant: "json", status: 201, family: "json", body: []byte(`{"kind":"json"}`), chunks: 1},
		{variant: "binary", status: 206, family: "binary", body: binaryBody, chunks: 2},
	}
	for _, test := range tests {
		t.Run(test.variant, func(t *testing.T) {
			operation := uploadResponseOperation(responses)
			service := &models.Service{BaseURL: provider.URL, DefaultHeaders: models.DefaultHeaders{"X-Variant": test.variant}}
			stream := &mockStream{}
			status, err := NewDispatcher().ExecuteStream(context.Background(), service, operation, map[string]any{
				"upload_mode": "multipart", "media": []byte("payload"),
			}, nil, nil, stream)
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			if status != test.status || !bytes.Equal(bytes.Join(stream.chunks, nil), test.body) {
				t.Fatalf("status=%d chunks=%d body=%q", status, len(stream.chunks), bytes.Join(stream.chunks, nil))
			}
			if len(stream.chunks) != test.chunks || len(stream.contracts) != 1 {
				t.Fatalf("chunks=%d contracts=%#v", len(stream.chunks), stream.contracts)
			}
			want := responseContractSignal{status: test.status, family: test.family}
			if stream.contracts[0] != want || stream.bodyBeforeContract {
				t.Fatalf("contract=%#v body_before_contract=%t", stream.contracts[0], stream.bodyBeforeContract)
			}
		})
	}
}

func TestUploadWorkflowResponseTelemetryIsBounded(t *testing.T) {
	const privateMedia = "application/vnd.tenant-private+json"
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previous)

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", privateMedia)
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"tenant_secret":"hidden"}`))
	}))
	defer server.Close()

	responses := models.Responses{"201": {Representations: []models.ResponseRepresentation{{MediaType: privateMedia}}}}
	ctx, span := provider.Tracer("test").Start(context.Background(), "engine.execution")
	_, err := NewDispatcher().ExecuteStream(ctx, &models.Service{BaseURL: server.URL}, uploadResponseOperation(responses), map[string]any{
		"upload_mode": "multipart", "media": []byte("payload"),
	}, nil, nil, &mockStream{})
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	span.End()

	attributes := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.execution"))
	if attributes["response.media_family"] != "json" || attributes["response.media_selection.outcome"] != "matched" {
		t.Fatalf("response attributes = %#v", attributes)
	}
	serialized := fmt.Sprint(attributes)
	for _, secret := range []string{"tenant-private", "tenant_secret", "hidden", server.URL} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("upload telemetry leaked %q: %s", secret, serialized)
		}
	}
}

func uploadResponseOperation(responses models.Responses) *models.IntegrationObject {
	workflow := &workflowcontract.UploadWorkflow{Version: workflowcontract.Version, AcceptedMediaTypes: []string{"application/octet-stream"}, Modes: []workflowcontract.UploadMode{{
		Kind: workflowcontract.UploadMultipart,
		Steps: []workflowcontract.UploadStep{{Kind: workflowcontract.StepTransfer, Method: http.MethodPost,
			URL: workflowcontract.URLSource{Kind: workflowcontract.URLDeclaredPath, Path: "/mixed"}, Body: workflowcontract.BodyMedia,
			SuccessStatuses: []workflowcontract.StatusRange{{Min: 200, Max: 299}}}},
	}}}
	return explicitAnonymousEndpoint(&models.IntegrationObject{
		Method: http.MethodPost, Responses: responses,
		RequestContent: uploadRequestContent(workflow),
	})
}

func uploadRequestContent(workflow *workflowcontract.UploadWorkflow) *models.RequestContent {
	return &models.RequestContent{
		PayloadParameter: "media", UploadWorkflow: workflow,
		Representations: []models.RequestRepresentation{{
			MediaType: "application/octet-stream", Serialization: models.RequestSerializationRaw,
			Schema: &models.SchemaContract{Projection: models.Schema{Type: "string", Format: "binary"}},
		}},
	}
}

func uploadVendorHandler(t *testing.T, attempts *int, baseURL func() string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*attempts++
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("attempt %d missing auth", *attempts)
		}
		switch *attempts {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
		case 2:
			w.Header().Set("Location", baseURL()+"/session")
			w.WriteHeader(http.StatusOK)
		case 3:
			w.WriteHeader(http.StatusInternalServerError)
		case 4:
			w.WriteHeader(308)
		default:
			w.WriteHeader(http.StatusCreated)
		}
	})
}

func uploadRetryRules() []retrypolicy.Rule {
	rules := retryV3Rules()
	action := retrypolicy.Action{MaxAttempts: 2, MaxElapsedMs: 1_000, Backoff: retrypolicy.Backoff{Strategy: retrypolicy.BackoffFixed, MaxDelayMs: 1}}
	return append(rules, retrypolicy.Rule{
		Predicates: retrypolicy.Predicates{
			Methods: []string{"PUT"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationWrite},
			Statuses: []retrypolicy.StatusRange{{Min: 500, Max: 599}}, BodyReplayability: retrypolicy.BodyReplayable,
			IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyRequired, Header: "Idempotency-Key"},
		},
		Action: action,
	})
}

func tinyDispatcherUploadWorkflow() *workflowcontract.UploadWorkflow {
	statuses := []workflowcontract.StatusRange{{Min: 200, Max: 299}}
	return &workflowcontract.UploadWorkflow{Version: 1, AcceptedMediaTypes: []string{"application/octet-stream"}, Modes: []workflowcontract.UploadMode{{Kind: workflowcontract.UploadResumable, Steps: []workflowcontract.UploadStep{
		{Kind: workflowcontract.StepInitiate, Method: "POST", URL: workflowcontract.URLSource{Kind: workflowcontract.URLDeclaredPath, Path: "/init"}, Body: workflowcontract.BodyMetadata, SuccessStatuses: statuses},
		{Kind: workflowcontract.StepTransfer, Method: "PUT", URL: workflowcontract.URLSource{Kind: workflowcontract.URLResponseHeader, HeaderName: "Location"}, Body: workflowcontract.BodyMedia, Chunking: &workflowcontract.Chunking{DefaultSizeBytes: 4, SizeMultipleBytes: 1, MaxSizeBytes: 4}, SuccessStatuses: statuses, ContinueStatuses: []workflowcontract.StatusRange{{Min: 308, Max: 308}}},
	}}}}
}

func uploadConcurrencyPolicy() *ratelimitpolicy.Config {
	return &ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{{
		Name: "upload_parallel", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests,
		Identity: ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: ratelimitpolicy.IdentityConnection}}},
		Cost:     ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmConcurrency, Concurrency: &ratelimitpolicy.Concurrency{Limit: 1},
	}}}
}
