package engine

import (
	"context"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestOpenAPI32QueryDispatchPreservesAttemptControls proves custom methods still pass through shared retry and quota controls.
func TestOpenAPI32QueryDispatchPreservesAttemptControls(t *testing.T) {
	attempts := 0
	provider := httptest.NewServer(openAPI32QueryProvider(t, &attempts))
	defer provider.Close()

	rateStore := &providerRateLimitStoreStub{}
	dispatcher := NewDispatcherWithProviderRateLimits(rateStore)
	service := &models.Service{BaseURL: provider.URL, ServiceVersionID: uuid.New(),
		AuthConfigs: models.AuthConfigs{{Name: "bearer", Type: "http", Scheme: "bearer"}},
		RetryConfig: &models.RetryConfig{Version: retrypolicy.Version, Rules: queryRetryRules()}, RateLimit: uploadConcurrencyPolicy(),
	}
	operation := &models.IntegrationObject{StableKey: "rest:QUERY:/search", Method: "QUERY", Path: "/search",
		Parameters:           models.Parameters{{Name: "filter", In: "querystring", Content: map[string]models.ParameterContent{"application/x-www-form-urlencoded": {}}}},
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "bearer"}}}},
	}
	timings := NewExecutionTimings()
	ctx, cancel := context.WithTimeout(WithProviderRateLimitIdentity(ContextWithExecutionTimings(context.Background(), timings), uuid.New(), uuid.New(), uuid.New()), time.Second)
	defer cancel()
	status, err := dispatcher.ExecuteStream(ctx, service, operation, map[string]any{"filter": map[string]any{"q": "a + b", "active": true}}, map[string]any{"bearer": "token"}, nil, NewBufferStream())
	if err != nil || status != http.StatusOK {
		t.Fatalf("QUERY status=%d err=%v", status, err)
	}
	if attempts != 2 || len(rateStore.requests) != 2 || len(rateStore.releases) != 2 || timings.Count("provider_attempt_count") != 2 {
		t.Fatalf("attempt controls = attempts:%d acquires:%d releases:%d timings:%d", attempts, len(rateStore.requests), len(rateStore.releases), timings.Count("provider_attempt_count"))
	}
}

// openAPI32QueryProvider checks exact wire bytes so serialization regressions cannot hide behind decoded query equality.
func openAPI32QueryProvider(t *testing.T, attempts *int) http.HandlerFunc {
	t.Helper()
	return func(response http.ResponseWriter, request *http.Request) {
		*attempts++
		if request.Method != "QUERY" || request.URL.RawQuery != "active=true&q=a+%2B+b" {
			t.Fatalf("provider request = %s %s", request.Method, request.URL.String())
		}
		if request.Header.Get("Authorization") != "Bearer token" {
			t.Fatal("provider request is missing canonical auth")
		}
		if *attempts == 1 {
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte("ok"))
	}
}

// queryRetryRules permits only the reviewed read-like custom method rather than widening retry eligibility globally.
func queryRetryRules() []retrypolicy.Rule {
	return []retrypolicy.Rule{{
		Predicates: retrypolicy.Predicates{Methods: []string{"QUERY"}, OperationKinds: []retrypolicy.OperationKind{retrypolicy.OperationRead},
			Statuses: []retrypolicy.StatusRange{{Min: 500, Max: 599}}, BodyReplayability: retrypolicy.BodyAny,
			IdempotencyKey: retrypolicy.IdempotencyKeyPredicate{Requirement: retrypolicy.IdempotencyKeyAny}},
		Action: retrypolicy.Action{MaxAttempts: 2, MaxElapsedMs: 1_000, Backoff: retrypolicy.Backoff{Strategy: retrypolicy.BackoffFixed}},
	}}
}

// TestOpenAPI32CustomMethodTelemetryIsBounded keeps arbitrary method tokens out of low-cardinality audit attributes.
func TestOpenAPI32CustomMethodTelemetryIsBounded(t *testing.T) {
	const customMethod = "CoPy-TenantSecret"
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != customMethod {
			t.Fatalf("custom method = %q", request.Method)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tracerProvider)
	defer otel.SetTracerProvider(previous)

	operation := explicitAnonymousEndpoint(&models.IntegrationObject{StableKey: "rest:custom:/copy", Method: customMethod, Path: "/copy"})
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation, nil, nil, nil, NewBufferStream())
	if err != nil || status != http.StatusOK {
		t.Fatalf("custom method status=%d err=%v", status, err)
	}
	attributes := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.dispatch.vendor_call"))
	if attributes["http.method_family"] != "custom" || strings.Contains(fmt.Sprint(attributes), customMethod) {
		t.Fatalf("custom method leaked to telemetry: %#v", attributes)
	}
	if boundedProviderMethod(customMethod) != "custom" {
		t.Fatal("custom method leaked to structured log family")
	}
}

// TestOpenAPI32PositionalMultipartStreamsOrderedParts protects order because positional multipart has no stable field names.
func TestOpenAPI32PositionalMultipartStreamsOrderedParts(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mediaType, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/mixed" {
			t.Fatalf("content type = %q err=%v", request.Header.Get("Content-Type"), err)
		}
		reader := multipart.NewReader(request.Body, parameters["boundary"])
		wantTypes := []string{"application/json", "image/png", "application/octet-stream", "application/octet-stream"}
		wantBodies := []string{"{\"name\":\"photo\"}\n", "png", "chunk-1", "chunk-2"}
		for index := range wantTypes {
			part, nextErr := reader.NextPart()
			if nextErr != nil {
				t.Fatalf("part %d: %v", index, nextErr)
			}
			body, _ := io.ReadAll(part)
			if part.Header.Get("Content-Type") != wantTypes[index] || string(body) != wantBodies[index] {
				t.Fatalf("part %d = type:%q body:%q", index, part.Header.Get("Content-Type"), body)
			}
		}
		if _, err := reader.NextPart(); err != io.EOF {
			t.Fatalf("unexpected trailing part: %v", err)
		}
		response.WriteHeader(http.StatusCreated)
	}))
	defer provider.Close()

	content := &models.RequestContent{PayloadParameter: "body", Representations: []models.RequestRepresentation{{
		MediaType: "multipart/mixed", Serialization: models.RequestSerializationMultipart,
		PrefixEncoding: []models.RequestEncoding{{ContentType: "application/json"}, {ContentType: "image/png"}},
		ItemEncoding:   &models.RequestEncoding{ContentType: "application/octet-stream"},
	}}}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{StableKey: "rest:POST:/media", Method: http.MethodPost, Path: "/media",
		Parameters: models.Parameters{{Name: "body", In: "body"}}, RequestContent: content,
	})
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation,
		map[string]any{"body": []any{map[string]any{"name": "photo"}, []byte("png"), []byte("chunk-1"), []byte("chunk-2")}}, nil, nil, NewBufferStream())
	if err != nil || status != http.StatusCreated {
		t.Fatalf("multipart status=%d err=%v", status, err)
	}
}

// TestOpenAPI32PositionalMultipartDefaultsRemainingEncoding allows unspecified trailing parts without inventing a media contract.
func TestOpenAPI32PositionalMultipartDefaultsRemainingEncoding(t *testing.T) {
	encoding, err := positionalEncoding([]models.RequestEncoding{{ContentType: "application/json"}}, nil, 2)
	if err != nil || !reflect.DeepEqual(encoding, models.RequestEncoding{}) {
		t.Fatalf("default positional encoding = %#v err=%v", encoding, err)
	}
}

// TestOpenAPI32PositionalMultipartStreamsOneNestedLevel enforces the reviewed nesting depth while retaining ordered framing.
func TestOpenAPI32PositionalMultipartStreamsOneNestedLevel(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, outerParameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("outer content type: %v", err)
		}
		outer := multipart.NewReader(request.Body, outerParameters["boundary"])
		part, err := outer.NextPart()
		if err != nil {
			t.Fatalf("outer part: %v", err)
		}
		mediaType, nestedParameters, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil || mediaType != "multipart/mixed" {
			t.Fatalf("nested content type = %q err=%v", part.Header.Get("Content-Type"), err)
		}
		assertNestedMultipartBodies(t, multipart.NewReader(part, nestedParameters["boundary"]), []string{"manifest", "chunk"})
		response.WriteHeader(http.StatusCreated)
	}))
	defer provider.Close()

	nested := models.RequestEncoding{ContentType: "multipart/mixed",
		PrefixEncoding: []models.RequestEncoding{{ContentType: "text/plain"}},
		ItemEncoding:   &models.RequestEncoding{ContentType: "application/octet-stream"},
	}
	content := &models.RequestContent{PayloadParameter: "body", Representations: []models.RequestRepresentation{{
		MediaType: "multipart/mixed", Serialization: models.RequestSerializationMultipart,
		PrefixEncoding: []models.RequestEncoding{nested},
	}}}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Method: http.MethodPost, Path: "/nested",
		Parameters: models.Parameters{{Name: "body", In: "body"}}, RequestContent: content,
	})
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation,
		map[string]any{"body": []any{[]any{"manifest", []byte("chunk")}}}, nil, nil, NewBufferStream())
	if err != nil || status != http.StatusCreated {
		t.Fatalf("nested multipart status=%d err=%v", status, err)
	}
}

// assertNestedMultipartBodies compares the stream sequentially because buffering into a map would erase positional semantics.
func assertNestedMultipartBodies(t *testing.T, reader *multipart.Reader, expected []string) {
	t.Helper()
	for index, wanted := range expected {
		part, err := reader.NextPart()
		if err != nil {
			t.Fatalf("nested part %d: %v", index, err)
		}
		body, err := io.ReadAll(part)
		if err != nil || string(body) != wanted {
			t.Fatalf("nested part %d body=%q err=%v", index, body, err)
		}
	}
	if _, err := reader.NextPart(); err != io.EOF {
		t.Fatalf("unexpected nested trailing part: %v", err)
	}
}

// TestOpenAPI32SequentialJSONRequestStreamsItems verifies item framing instead of accepting an equivalent in-memory array.
func TestOpenAPI32SequentialJSONRequestStreamsItems(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if string(body) != "{\"id\":1}\n{\"id\":2}\n" {
			t.Fatalf("sequential body = %q", body)
		}
		response.WriteHeader(http.StatusAccepted)
	}))
	defer provider.Close()
	content := &models.RequestContent{PayloadParameter: "body", Representations: []models.RequestRepresentation{{
		MediaType: "application/x-ndjson", Serialization: models.RequestSerializationRaw, ItemSchema: &models.SchemaContract{},
	}}}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Method: http.MethodPost, Path: "/events", Parameters: models.Parameters{{Name: "body", In: "body"}}, RequestContent: content})
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: provider.URL}, operation,
		map[string]any{"body": []any{map[string]any{"id": 1}, map[string]any{"id": 2}}}, nil, nil, NewBufferStream())
	if err != nil || status != http.StatusAccepted {
		t.Fatalf("sequential status=%d err=%v", status, err)
	}
}
