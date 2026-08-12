package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type mockStream struct {
	chunks             [][]byte
	contracts          []responseContractSignal
	bodyBeforeContract bool
}

type responseContractSignal struct {
	status int
	family string
}

func testRequestContent(mediaType, serialization string) *models.RequestContent {
	return &models.RequestContent{Representations: []models.RequestRepresentation{{
		MediaType: mediaType, Serialization: serialization,
		Schema: &models.SchemaContract{Projection: models.Schema{Type: "object", AdditionalProperties: &models.Schema{}}},
	}}}
}

func testRequestContentWithSchema(mediaType, serialization string, schema models.Schema) *models.RequestContent {
	content := testRequestContent(mediaType, serialization)
	content.Representations[0].Schema = &models.SchemaContract{Projection: schema}
	return content
}

func rawTestRequestContent(mediaType string, binary bool) *models.RequestContent {
	schema := models.Schema{Type: "string"}
	if binary {
		schema.Format = "binary"
	}
	content := testRequestContentWithSchema(mediaType, models.RequestSerializationRaw, schema)
	content.PayloadParameter = "body"
	return content
}

func TestPrepareRequestPartsBindingPrecedence(t *testing.T) {
	srv := &models.Service{BaseURL: "https://service.example.com"}
	obj := &models.IntegrationObject{
		Path: "/accounts/{accountId}", Method: http.MethodGet,
		Parameters: models.Parameters{
			{Name: "accountId", In: "path"}, {Name: "portal", In: "query"}, {Name: "tenant", In: "query"}, {Name: "X-Tenant", In: "header"},
		},
	}
	bindings := []store.BucketValue{
		{Location: "base_url", Value: "https://resource.example.com", Mode: "force", SourceKind: "connection_resource"},
		{Location: "path", KeyName: "accountId", Value: "forced-account", Mode: "force"},
		{Location: "query", KeyName: "portal", Value: "default-portal", Mode: "default"},
		{Location: "query", KeyName: "tenant", Value: "forced-tenant", Mode: "force"},
		{Location: "header", KeyName: "X-Tenant", Value: "forced-header", Mode: "force"},
	}
	params := map[string]any{"accountId": "caller-account", "portal": "caller-portal", "tenant": "caller-tenant", "X-Tenant": "caller-header"}
	reqURL, headers, _, err := prepareRequestParts(srv, obj, params, bindings)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	if reqURL != "https://resource.example.com/accounts/forced-account?portal=caller-portal&tenant=forced-tenant" {
		t.Fatalf("request URL = %q", reqURL)
	}
	if headers["X-Tenant"] != "forced-header" {
		t.Fatalf("forced header = %q", headers["X-Tenant"])
	}
}

func TestPrepareRequestPartsPathBindingPreservesReviewedSlashes(t *testing.T) {
	service := &models.Service{BaseURL: "https://service.example.com"}
	endpoint := &models.IntegrationObject{
		Path: "/files/{resource}", Method: http.MethodGet,
		Parameters: models.Parameters{{
			Name: "resource", In: "path", PathEncoding: models.PathEncodingPreserveSlashes,
		}},
	}
	binding := store.BucketValue{Location: "path", KeyName: "resource", Value: "folders/a b?c#d", Mode: "force"}
	requestURL, _, _, err := prepareRequestParts(service, endpoint, nil, []store.BucketValue{binding})
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	if requestURL != "https://service.example.com/files/folders/a%20b%3Fc%23d" {
		t.Fatalf("request URL = %q", requestURL)
	}
}

func TestDispatcherExecuteStream_PathEncodingIsParameterDriven(t *testing.T) {
	tests := []struct {
		name         string
		pathEncoding string
		wantPath     string
	}{
		{name: "default escapes slash", wantPath: "/files/folders%2Fa%20b%3Fc%23d"},
		{name: "preserve slashes", pathEncoding: models.PathEncodingPreserveSlashes, wantPath: "/files/folders/a%20b%3Fc%23d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestURI string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestURI = r.RequestURI
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			endpoint := &models.IntegrationObject{
				Path: "/files/{resource}", Method: http.MethodGet,
				Parameters: models.Parameters{{Name: "resource", In: "path", PathEncoding: tt.pathEncoding}},
			}
			_, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: server.URL}, explicitAnonymousEndpoint(endpoint), map[string]any{"resource": "folders/a b?c#d"}, nil, nil, &mockStream{})
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			if requestURI != tt.wantPath {
				t.Fatalf("provider request URI = %q, want %q", requestURI, tt.wantPath)
			}
		})
	}
}

// TestBindingBaseURLRejectsLiteralProvenance is the dispatch-time backstop for
// databases that still contain a legacy literal base_url row during upgrade.
func TestBindingBaseURLRejectsLiteralProvenance(t *testing.T) {
	got := bindingBaseURL("https://service.example.com", []store.BucketValue{{
		Location: "base_url", Value: "https://attacker.example", Mode: "force", SourceKind: "literal",
	}})
	if got != "https://service.example.com" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestPrepareRequestPartsForcedQueryReplacesRatherThanDuplicates(t *testing.T) {
	srv := &models.Service{BaseURL: "https://service.example.com"}
	obj := &models.IntegrationObject{Path: "/accounts", Method: http.MethodGet, Parameters: models.Parameters{{Name: "portal", In: "query"}}}
	binding := store.BucketValue{Location: "query", KeyName: "portal", Value: "resource-portal", Mode: "force"}
	reqURL, _, _, err := prepareRequestParts(srv, obj, map[string]any{"portal": "caller-portal"}, []store.BucketValue{binding})
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	if strings.Count(reqURL, "portal=") != 1 || !strings.Contains(reqURL, "portal=resource-portal") {
		t.Fatalf("forced query did not replace caller value: %q", reqURL)
	}
}

func TestPrepareRequestPartsRejectsUnsafeResolvedHeaders(t *testing.T) {
	srv := &models.Service{BaseURL: "https://service.example.com"}
	obj := &models.IntegrationObject{Path: "/accounts", Method: http.MethodGet}
	tests := []store.BucketValue{
		{Location: "header", KeyName: "Authorization", Value: "shadow", Mode: "force"},
		{Location: "header", KeyName: "X-Tenant", Value: "safe\r\nInjected: true", Mode: "force"},
	}
	for _, binding := range tests {
		if _, _, _, err := prepareRequestParts(srv, obj, nil, []store.BucketValue{binding}); err == nil {
			t.Fatalf("unsafe resolved header was accepted: %#v", binding)
		}
	}
}

func (m *mockStream) Send(chunk []byte) error {
	if len(m.contracts) == 0 {
		m.bodyBeforeContract = true
	}
	copied := make([]byte, len(chunk))
	copy(copied, chunk)
	m.chunks = append(m.chunks, copied)
	return nil
}

func (m *mockStream) SendResponseContract(status int, mediaFamily string) error {
	m.contracts = append(m.contracts, responseContractSignal{status: status, family: mediaFamily})
	return nil
}

func TestWaitForRetryStopsWhenExecutionContextExpires(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := waitForRetry(ctx, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForRetry error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("retry backoff ignored cancellation; elapsed=%s", elapsed)
	}
}

func TestDispatcherExecuteStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}
	stream := &mockStream{}

	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("Expected status 200, got %d", status)
	}

	// Verify chunks
	var fullResponse []byte
	for _, chunk := range stream.chunks {
		fullResponse = append(fullResponse, chunk...)
	}

	if !bytes.Equal(fullResponse, []byte(`{"status":"success"}`)) {
		t.Errorf("Expected full response, got `%s`", string(fullResponse))
	}
}

func TestDispatcherExecuteStream_RecordsProviderTimings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()

	timings := NewExecutionTimings()
	ctx := ContextWithExecutionTimings(context.Background(), timings)
	stream := &mockStream{}
	status, err := NewDispatcher().ExecuteStream(
		ctx,
		&models.Service{BaseURL: server.URL},
		explicitAnonymousEndpoint(&models.IntegrationObject{Path: "/test", Method: http.MethodGet}),
		nil,
		nil,
		nil,
		stream,
	)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status: got %d, want %d", status, http.StatusOK)
	}

	snapshot := timings.SnapshotMilliseconds()
	for _, key := range []string{
		"provider_request_prepare",
		"provider_time_to_headers",
		"provider_body_stream",
		"provider_body_read",
		"provider_total",
	} {
		if _, ok := snapshot[key]; !ok {
			t.Fatalf("missing timing %q in %v", key, snapshot)
		}
	}
}

func TestNewDispatcher_UsesTunedKeepAliveTransport(t *testing.T) {
	d := NewDispatcher()
	transport, ok := d.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("dispatcher transport = %T, want *http.Transport", d.client.Transport)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 must be enabled for HTTPS providers")
	}
	if transport.MaxIdleConns < 200 || transport.MaxIdleConnsPerHost < 50 {
		t.Fatalf("idle pool too small: max=%d per_host=%d", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}
	if transport.IdleConnTimeout < 90*time.Second {
		t.Fatalf("IdleConnTimeout = %s, want at least 90s", transport.IdleConnTimeout)
	}
}

func TestApplyAuthAcceptsOIDCAlias(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "https://api.example.test/profile", nil)
	auths := models.AuthConfigs{{Type: "oidc", Name: "oidc_token"}}

	applySelectedAuth(req, auths, map[string]any{"oidc_token": "id-token"})

	if got := req.Header.Get("Authorization"); got != "Bearer id-token" {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
}

func TestApplyHTTPBasicAuthRequiresUsernameAndPassword(t *testing.T) {
	auths := models.AuthConfigs{{Type: "http", Scheme: "basic", Name: "basicAuth", BasicPasswordMode: authrouting.BasicPasswordRequired}}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "basicAuth"}}}}
	tests := []struct {
		name        string
		credentials map[string]any
		wantHeader  bool
	}{
		{
			name: "complete pair",
			credentials: map[string]any{
				"basicAuth_username": "alice",
				"basicAuth_password": "s3cr3t",
			},
			wantHeader: true,
		},
		{
			name:        "username only",
			credentials: map[string]any{"basicAuth_username": "alice"},
		},
		{
			name:        "password only",
			credentials: map[string]any{"basicAuth_password": "s3cr3t"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "https://api.example.test/profile", nil)
			selected, _ := selectRequestAuth(auths, requirements, tt.credentials)
			applySelectedAuth(req, selected, tt.credentials)
			if gotHeader := req.Header.Get("Authorization") != ""; gotHeader != tt.wantHeader {
				t.Fatalf("Authorization present = %v, want %v", gotHeader, tt.wantHeader)
			}
		})
	}
}

func TestDispatcherExecuteStream_LogsProviderConnectionReuse(t *testing.T) {
	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{Name: "GitHub", BaseURL: server.URL}
	obj := &models.IntegrationObject{Name: "repos.list", Path: "/test", Method: http.MethodGet}
	for i := 0; i < 2; i++ {
		if _, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, &mockStream{}); err != nil {
			t.Fatalf("ExecuteStream call %d failed: %v", i+1, err)
		}
	}

	if !strings.Contains(logs.String(), "reused=true") {
		t.Fatalf("expected provider connection reuse log, got:\n%s", logs.String())
	}
}

// TestDispatcherExecuteStream_Retry verifies that a service with an explicit
// RetryConfig retries a 500 and succeeds on the second attempt. Retry is
// strictly config-driven now (no hardcoded fallback), so the config must be
// set explicitly -- see TestDispatcherExecuteStream_NoRetryWithoutConfig for
// the converse.
func TestDispatcherExecuteStream_Retry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`Internal error`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"success"}`))
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}, DefaultHeaders: models.DefaultHeaders{"Idempotency-Key": "stable-test-key"}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("Expected status 200, got %d", status)
	}
	if attempts != 2 {
		t.Errorf("Expected 2 attempts, got %d", attempts)
	}
}

// The REST retry safety gate is reused so GraphQL mutations do not gain a
// separate, less conservative execution path.
func TestDispatcherExecuteStream_GraphQLReusesRESTExecutionSystem(t *testing.T) {
	attempts := 0
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":{"viewer":{"login":"octocat"}}}`))
	}))
	defer server.Close()

	d := NewDispatcher()
	query := `query Viewer($id: ID!) { viewer(id: $id) { login } }`
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}, DefaultHeaders: models.DefaultHeaders{"Idempotency-Key": "stable-test-key"}}
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query, ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindQuery, RequestContent: testRequestContent("application/json", models.RequestSerializationJSON)}
	params := map[string]any{"id": "u_123"}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), params, nil, nil, stream)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry reused), got %d", attempts)
	}
	if gotBody["query"] != query {
		t.Errorf("query = %v, want %v", gotBody["query"], query)
	}
	variables, _ := gotBody["variables"].(map[string]any)
	if variables["id"] != "u_123" {
		t.Errorf("variables[id] = %v, want u_123", variables["id"])
	}
	if len(stream.chunks) != 1 || string(stream.chunks[0]) != `{"data":{"viewer":{"login":"octocat"}}}` {
		t.Errorf("unexpected streamed response: %#v", stream.chunks)
	}
}

func TestDispatcherExecuteStream_GraphQLPOSTWithoutIdempotencyKey_NoRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	d := NewDispatcher()
	query := `mutation CreateWidget($name: String!) { createWidget(name: $name) { id } }`
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}}
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query, ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindMutation, RequestContent: testRequestContent("application/json", models.RequestSerializationJSON)}

	stream := &mockStream{}
	_, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), map[string]any{"name": "widget"}, nil, nil, stream)
	if err == nil {
		t.Fatalf("expected an error from the unretried 500")
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt (no idempotency key), got %d", attempts)
	}
}

func TestDispatcherExecuteStream_ExplicitGraphQLProtocolRequiresDocument(t *testing.T) {
	d := NewDispatcher()
	srv := &models.Service{BaseURL: "https://provider.example"}
	obj := &models.IntegrationObject{
		Path: "/graphql", Method: http.MethodPost,
		ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindQuery,
	}

	_, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, &mockStream{})

	if err == nil || !strings.Contains(err.Error(), "missing its query document") {
		t.Fatalf("expected missing GraphQL document error, got %v", err)
	}
}

func TestDispatcherGraphQLExecutionRecordsSafeProtocolAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	defer otel.SetTracerProvider(previousProvider)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"1"}}}`))
	}))
	defer server.Close()
	query := `query Viewer { viewer { id } }`
	obj := &models.IntegrationObject{
		Name: "viewer", Method: http.MethodPost, Path: "/graphql", GraphQLQuery: &query,
		ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindQuery,
	}
	ctx, executionSpan := provider.Tracer("test").Start(context.Background(), "engine.execution")
	if _, err := NewDispatcher().ExecuteStream(ctx, &models.Service{Name: "test", BaseURL: server.URL}, explicitAnonymousEndpoint(obj), nil, nil, nil, &mockStream{}); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	executionSpan.End()

	executionAttributes := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.execution"))
	if executionAttributes["request.serialization"] != "graphql" || executionAttributes["request.media_family"] != "json" {
		t.Fatalf("unexpected request content attributes: %#v", executionAttributes)
	}
	providerAttributes := safeStringSpanAttributes(t, recordedSpan(t, recorder.Ended(), "engine.dispatch.vendor_call"))
	if providerAttributes["provider.protocol"] != "graphql" || providerAttributes["graphql.operation.kind"] != "query" {
		t.Fatalf("unexpected protocol attributes: %#v", providerAttributes)
	}
}

func recordedSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q was not recorded", name)
	return nil
}

func safeStringSpanAttributes(t *testing.T, span sdktrace.ReadOnlySpan) map[string]string {
	t.Helper()
	attributes := make(map[string]string, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		key := string(attr.Key)
		if isSensitiveRequestAttribute(key) {
			t.Fatalf("sensitive request attribute recorded: %s", key)
		}
		attributes[key] = attr.Value.AsString()
	}
	return attributes
}

func isSensitiveRequestAttribute(key string) bool {
	for _, fragment := range []string{"body", "header", "query", "variables"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func TestRequestContentTelemetryUsesBoundedValues(t *testing.T) {
	query := "query Viewer { viewer { id } }"
	tests := []struct {
		name          string
		operation     *models.IntegrationObject
		serialization string
		mediaFamily   string
	}{
		{name: "no REST body", operation: &models.IntegrationObject{}, serialization: "none", mediaFamily: "none"},
		{name: "JSON", operation: &models.IntegrationObject{RequestContent: testRequestContent("application/vnd.vendor+json", models.RequestSerializationJSON)}, serialization: "json", mediaFamily: "json"},
		{name: "form", operation: &models.IntegrationObject{RequestContent: testRequestContent("application/x-www-form-urlencoded", models.RequestSerializationForm)}, serialization: "form_urlencoded", mediaFamily: "form"},
		{name: "multipart", operation: &models.IntegrationObject{RequestContent: testRequestContent("multipart/form-data", models.RequestSerializationMultipart)}, serialization: "multipart", mediaFamily: "multipart"},
		{name: "raw text", operation: &models.IntegrationObject{RequestContent: testRequestContent("text/plain; charset=utf-8", models.RequestSerializationRaw)}, serialization: "raw", mediaFamily: "text"},
		{name: "raw binary", operation: &models.IntegrationObject{RequestContent: testRequestContent("application/octet-stream", models.RequestSerializationRaw)}, serialization: "raw", mediaFamily: "binary"},
		{name: "invalid representation", operation: &models.IntegrationObject{RequestContent: testRequestContent("application/vnd.unbounded", "provider-specific")}, serialization: "none", mediaFamily: "none"},
		{name: "GraphQL", operation: &models.IntegrationObject{GraphQLQuery: &query, ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindQuery}, serialization: "graphql", mediaFamily: "json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			serialization, mediaFamily := requestContentTelemetry(tt.operation)
			if serialization != tt.serialization || mediaFamily != tt.mediaFamily {
				t.Fatalf("requestContentTelemetry() = (%q, %q), want (%q, %q)", serialization, mediaFamily, tt.serialization, tt.mediaFamily)
			}
		})
	}
}

// TestDispatcherExecuteStream_NoRetryWithoutConfig is the strict half of the
// "no hardcoded retries" rule: a service with no RetryConfig gets exactly
// one attempt, even on a 500.
func TestDispatcherExecuteStream_NoRetryWithoutConfig(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`Internal error`))
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("expected the single provider response, status=%d err=%v", status, err)
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt with no retry config, got %d", attempts)
	}
}

// TestDispatcherExecuteStream_POSTWithoutIdempotencyKey_NoRetry verifies the
// method-safety gate: a POST is not guaranteed idempotent by HTTP semantics,
// so even with a RetryConfig allowing retries, a 5xx on a POST with no
// idempotency key in context fails fast on the single attempt rather than
// risking a double-fired side effect.
func TestDispatcherExecuteStream_POSTWithoutIdempotencyKey_NoRetry(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodPost}

	stream := &mockStream{}
	_, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err == nil {
		t.Fatal("expected an error from the single failed attempt")
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt for a POST with no idempotency key, got %d", attempts)
	}
}

// TestDispatcherExecuteStream_POSTWithIdempotencyKey_Retries verifies a POST
// still retries on 5xx as configured when an idempotency key is present --
// the gate only disables retry when there's no key to dedupe on.
func TestDispatcherExecuteStream_POSTWithIdempotencyKey_Retries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}, DefaultHeaders: models.DefaultHeaders{"Idempotency-Key": "stable-test-key"}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodPost}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry allowed with idempotency key present), got %d", attempts)
	}
}

// TestDispatcherExecuteStream_GETWithoutIdempotencyKey_StillRetries verifies
// the method-safety gate only applies to non-idempotent methods: a GET
// retries on 5xx regardless of whether an idempotency key is present, since
// repeating a GET is always safe.
func TestDispatcherExecuteStream_GETWithoutIdempotencyKey_StillRetries(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{Version: 3, Rules: retryV3Rules()}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (GET is always safe to retry), got %d", attempts)
	}
}

func TestDispatcherExecuteStream_Paginated(t *testing.T) {
	caseData := v3TokenCase()
	pagesServed := 0
	// Mock a server that serves exactly 2 pages using cursor parameter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pagesServed++
		cursor := r.URL.Query().Get("pageToken")
		w.WriteHeader(http.StatusOK)

		if cursor == "" {
			w.Write([]byte(`{"items":["item1"],"next":"page2"}`))
		} else if cursor == "page2" {
			w.Write([]byte(`{"items":["item2"]}`))
		} else {
			t.Errorf("Unexpected cursor: %s", cursor)
		}
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{
		Path:       "/test",
		Method:     http.MethodGet,
		Parameters: caseData.object.Parameters,
		Pagination: modelPolicy(caseData.policy),
	}
	stream := &mockStream{}

	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream Paginated failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("Expected status 200, got %d", status)
	}
	if pagesServed != 2 {
		t.Errorf("Expected 2 pages served, got %d", pagesServed)
	}

	var fullResponse []byte
	for _, chunk := range stream.chunks {
		fullResponse = append(fullResponse, chunk...)
	}

	respStr := string(fullResponse)
	if !strings.Contains(respStr, "item1") || !strings.Contains(respStr, "item2") {
		t.Errorf("Stream did not contain both items. Got: %s", respStr)
	}
}

// TestParseSSEEvents_MultiEvent validates that three distinct SSE events are
// each forwarded as a separate chunk with the `data:` prefix stripped.
func TestParseSSEEvents_MultiEvent(t *testing.T) {
	raw := "data: {\"n\":1}\n\ndata: {\"n\":2}\n\ndata: {\"n\":3}\n\n"
	stream := &mockStream{}

	if err := parseSSEEvents(strings.NewReader(raw), stream); err != nil {
		t.Fatalf("parseSSEEvents returned error: %v", err)
	}

	if len(stream.chunks) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(stream.chunks))
	}
	if string(stream.chunks[0]) != `{"n":1}` {
		t.Errorf("chunk 0: expected {\"n\":1}, got %s", stream.chunks[0])
	}
	if string(stream.chunks[1]) != `{"n":2}` {
		t.Errorf("chunk 1: expected {\"n\":2}, got %s", stream.chunks[1])
	}
	if string(stream.chunks[2]) != `{"n":3}` {
		t.Errorf("chunk 2: expected {\"n\":3}, got %s", stream.chunks[2])
	}
}

// TestParseSSEEvents_DoneSentinel validates that [DONE] stops the stream
// without forwarding the sentinel itself or any events after it.
func TestParseSSEEvents_DoneSentinel(t *testing.T) {
	raw := "data: {\"n\":1}\n\ndata: [DONE]\n\ndata: {\"n\":2}\n\n"
	stream := &mockStream{}

	done := "[DONE]"
	contract := &models.SSEResponseContract{ItemMode: "data", DoneSentinel: &done}
	if err := parseSSEEvents(strings.NewReader(raw), stream, contract); err != nil {
		t.Fatalf("parseSSEEvents returned error: %v", err)
	}

	// Only the first event should be forwarded; [DONE] stops parsing.
	if len(stream.chunks) != 1 {
		t.Fatalf("expected 1 chunk before [DONE], got %d", len(stream.chunks))
	}
	if string(stream.chunks[0]) != `{"n":1}` {
		t.Errorf("expected {\"n\":1}, got %s", stream.chunks[0])
	}
}

func TestParseSSEEvents_UndeclaredSentinelIsData(t *testing.T) {
	stream := &mockStream{}
	if err := parseSSEEvents(strings.NewReader("data: [DONE]\n\n"), stream); err != nil {
		t.Fatalf("parseSSEEvents returned error: %v", err)
	}
	if len(stream.chunks) != 1 || string(stream.chunks[0]) != "[DONE]" {
		t.Fatalf("undeclared sentinel was not preserved: %#v", stream.chunks)
	}
}

func TestParseSSEEvents_DataOnlyIncludesEmptyAndIgnoresMetadata(t *testing.T) {
	raw := ": comment\nevent: update\nid: 42\nretry: 1000\ndata:\n\n"
	stream := &mockStream{}
	if err := parseSSEEvents(strings.NewReader(raw), stream); err != nil {
		t.Fatalf("parseSSEEvents returned error: %v", err)
	}
	if len(stream.chunks) != 1 || string(stream.chunks[0]) != `""` {
		t.Fatalf("expected one empty decoded data item, got %#v", stream.chunks)
	}
}

// TestParseSSEEvents_MultiLineData validates that multi-line data fields
// within a single event are joined with a newline before being streamed.
func TestParseSSEEvents_MultiLineData(t *testing.T) {
	raw := "data: line1\ndata: line2\n\n"
	stream := &mockStream{}

	if err := parseSSEEvents(strings.NewReader(raw), stream); err != nil {
		t.Fatalf("parseSSEEvents returned error: %v", err)
	}

	if len(stream.chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(stream.chunks))
	}
	if string(stream.chunks[0]) != "line1\nline2" {
		t.Errorf("expected joined lines, got %s", stream.chunks[0])
	}
}

// TestDispatcherExecuteStream_SSE validates that the matched response
// representation drives framing and Accept negotiation.
func TestDispatcherExecuteStream_SSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("expected Accept: text/event-stream, got %s", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{
		Path:   "/stream",
		Method: http.MethodGet,
		Responses: models.Responses{"200": {Representations: []models.ResponseRepresentation{{
			MediaType: "text/event-stream", SSE: &models.SSEResponseContract{ItemMode: "data", DoneSentinel: stringPointer("[DONE]")},
		}}}},
	}
	stream := &mockStream{}

	status, err := d.ExecuteStream(context.Background(), srv, explicitAnonymousEndpoint(obj), nil, nil, nil, stream)
	if err != nil {
		t.Fatalf("SSE ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected 200, got %d", status)
	}
	// Two events before [DONE].
	if len(stream.chunks) != 2 {
		t.Fatalf("expected 2 SSE chunks, got %d: %v", len(stream.chunks), stream.chunks)
	}
	if string(stream.chunks[0]) != `{"chunk":1}` {
		t.Errorf("chunk 0: got %s", stream.chunks[0])
	}
	if string(stream.chunks[1]) != `{"chunk":2}` {
		t.Errorf("chunk 1: got %s", stream.chunks[1])
	}
}

func TestDispatcherExecuteStream_SelectsDecoderFromActualStatusAndMedia(t *testing.T) {
	done := "[END]"
	operation := &models.IntegrationObject{
		Path: "/mixed", Method: http.MethodGet,
		Parameters: models.Parameters{{Name: "X-Variant", In: "header"}},
		Responses: models.Responses{
			"200": {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream", SSE: &models.SSEResponseContract{ItemMode: "data", DoneSentinel: &done}}}},
			"201": {Representations: []models.ResponseRepresentation{{MediaType: "application/vnd.acme+json"}}},
			"206": {Representations: []models.ResponseRepresentation{{MediaType: "application/octet-stream"}}},
		},
	}
	binaryBody := bytes.Repeat([]byte{0x00, 0xff, 0x7f}, 2000)
	provider := httptest.NewServer(mixedResponseProvider(t, binaryBody))
	defer provider.Close()

	tests := []mixedResponseCase{
		{"sse", 200, "sse", []byte("one\ntwo"), 1}, {"json", 201, "json", []byte(`{"kind":"json"}`), 1}, {"binary", 206, "binary", binaryBody, 2},
	}
	for _, test := range tests {
		t.Run(test.variant, func(t *testing.T) { assertMixedResponseCase(t, provider.URL, operation, test) })
	}
}

type mixedResponseCase struct {
	variant string
	status  int
	family  string
	body    []byte
	chunks  int
}

func mixedResponseProvider(t *testing.T, binaryBody []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Accept") != "application/octet-stream, application/vnd.acme+json, text/event-stream" {
			t.Errorf("unexpected Accept header: %q", request.Header.Get("Accept"))
		}
		switch request.Header.Get("X-Variant") {
		case "sse":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: update\ndata: one\ndata: two\n\ndata: [END]\n\n")
		case "json":
			w.Header().Set("Content-Type", "application/vnd.acme+json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"kind":"json"}`)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(binaryBody)
		}
	}
}

func assertMixedResponseCase(t *testing.T, providerURL string, operation *models.IntegrationObject, test mixedResponseCase) {
	stream := &mockStream{}
	status, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{BaseURL: providerURL}, explicitAnonymousEndpoint(operation), map[string]any{"X-Variant": test.variant}, nil, nil, stream)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	if status != test.status || !bytes.Equal(bytes.Join(stream.chunks, nil), test.body) {
		t.Fatalf("status=%d chunks=%d body=%q", status, len(stream.chunks), bytes.Join(stream.chunks, nil))
	}
	if len(stream.chunks) != test.chunks {
		t.Fatalf("chunks=%d, want %d", len(stream.chunks), test.chunks)
	}
	if len(stream.contracts) != 1 || stream.contracts[0] != (responseContractSignal{status: test.status, family: test.family}) {
		t.Fatalf("response contract signals = %#v", stream.contracts)
	}
	if stream.bodyBeforeContract {
		t.Fatal("body was sent before the response contract signal")
	}
}

func TestPrepareRequestParts_PathQueryHeaderSeparation(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/users/{user_id}/repos",
		Method: http.MethodGet,
		Parameters: []models.Parameter{
			{Name: "user_id", In: "path"},
			{Name: "sort", In: "query"},
			{Name: "X-Custom-Header", In: "header"},
		},
	}
	params := map[string]any{
		"user_id":         "octocat",
		"sort":            "desc",
		"X-Custom-Header": "foo",
	}

	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}

	if !strings.HasPrefix(reqURL, "https://api.example.com/users/octocat/repos?") {
		t.Errorf("Path parameter not correctly replaced. Got: %s", reqURL)
	}
	if !strings.Contains(reqURL, "sort=desc") {
		t.Errorf("Query parameter 'sort' missing. Got: %s", reqURL)
	}
	if headers["X-Custom-Header"] != "foo" {
		t.Errorf("Header parameter missing or incorrect. Got: %s", headers["X-Custom-Header"])
	}
	if bodyReader != nil {
		t.Errorf("Expected nil bodyReader for GET request, got non-nil")
	}
}

func TestPrepareRequestPartsRejectsUndeclaredHEADParameter(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{Path: "/health", Method: http.MethodHead}

	if _, _, _, err := prepareRequestParts(srv, obj, map[string]any{"probe": "ready"}, nil); err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "probe"`) {
		t.Fatalf("undeclared HEAD parameter error = %v", err)
	}
}

func TestPrepareRequestParts_FormURLEncoded(t *testing.T) {
	srv := &models.Service{
		BaseURL: "https://api.example.com",
		DefaultHeaders: models.DefaultHeaders{
			"Content-Type": "application/x-www-form-urlencoded",
		},
	}
	obj := &models.IntegrationObject{
		Path:   "/charges",
		Method: http.MethodPost,
		RequestContent: testRequestContent(
			"application/x-www-form-urlencoded", models.RequestSerializationForm,
		),
	}
	params := map[string]any{
		"amount":   2000,
		"currency": "usd",
	}

	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}

	if reqURL != "https://api.example.com/charges" {
		t.Errorf("Unexpected URL: %s", reqURL)
	}
	if headers["Content-Type"] != "application/x-www-form-urlencoded" {
		t.Errorf("Expected application/x-www-form-urlencoded, got %s", headers["Content-Type"])
	}

	if bodyReader == nil {
		t.Fatalf("Expected non-nil bodyReader")
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(bodyReader)
	bodyStr := buf.String()

	if !strings.Contains(bodyStr, "amount=2000") || !strings.Contains(bodyStr, "currency=usd") {
		t.Errorf("Form encoded body missing parameters. Got: %s", bodyStr)
	}
}

func TestPrepareRequestPartsRejectsUndeclaredPathPlaceholders(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/repos/{owner}/{repo}/issues",
		Method: http.MethodPost,
		RequestContent: testRequestContent(
			"application/json", models.RequestSerializationJSON,
		),
		// Explicitly simulate missing definitions in obj.Parameters
		Parameters: []models.Parameter{},
	}

	params := map[string]any{
		"Owner": "test-owner", // Different casing than URL
		"Repo":  "test-repo",  // Different casing than URL
		"title": "Bug report",
	}

	if _, _, _, err := prepareRequestParts(srv, obj, params, nil); err == nil || !strings.Contains(err.Error(), `undeclared or unresolved path parameter "owner"`) {
		t.Fatalf("undeclared path placeholder error = %v", err)
	}
}

func TestPrepareRequestParts_HeaderInjection(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/test",
		Method: http.MethodGet,
	}

	params := map[string]any{
		"_headers": map[string]any{
			"X-Stripe-Version": "2023-10-16",
			"User-Agent":       "Fused-Agent",
		},
	}

	_, headers, _, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}

	if headers["X-Stripe-Version"] != "2023-10-16" {
		t.Errorf("Expected X-Stripe-Version header to be injected")
	}
	if headers["User-Agent"] != "Fused-Agent" {
		t.Errorf("Expected User-Agent header to be injected")
	}
}

func TestPrepareRequestParts_DynamicFormURLEncoded(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	// The reviewed operation, not a service default, selects form serialization.
	obj := &models.IntegrationObject{
		Path:   "/test",
		Method: http.MethodPost,
		RequestContent: testRequestContent(
			"application/x-www-form-urlencoded", models.RequestSerializationForm,
		),
	}

	params := map[string]any{
		"amount": 1000,
	}

	_, headers, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}

	if headers["Content-Type"] != "application/x-www-form-urlencoded" {
		t.Errorf("Expected Content-Type to be set to application/x-www-form-urlencoded")
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(bodyReader)
	if buf.String() != "amount=1000" {
		t.Errorf("Expected form-encoded payload, got: %s", buf.String())
	}
}

func TestPrepareRequestParts_PreservesReviewedJSONMediaType(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/widgets",
		Method: http.MethodPost,
		RequestContent: testRequestContent(
			"application/vnd.api+json", models.RequestSerializationJSON,
		),
	}

	_, headers, bodyReader, err := prepareRequestParts(srv, obj, map[string]any{
		"name": "one", "_headers": map[string]any{"content-type": "text/plain"},
	}, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}
	if headers["Content-Type"] != obj.RequestContent.Representations[0].MediaType || headers["content-type"] != "" {
		t.Fatalf("headers = %#v, want authoritative imported media type", headers)
	}
	var body map[string]any
	if err := json.NewDecoder(bodyReader).Decode(&body); err != nil {
		t.Fatalf("body was not valid JSON: %v", err)
	}
	if body["name"] != "one" {
		t.Fatalf("body = %#v, want imported request parameters", body)
	}
}

func TestDispatcherExecuteStream_RequestContentOverridesServiceDefault(t *testing.T) {
	var gotContentType string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	dispatcher := NewDispatcher()
	service := &models.Service{
		BaseURL:        server.URL,
		DefaultHeaders: models.DefaultHeaders{"Content-Type": "application/x-www-form-urlencoded"},
	}
	endpoint := &models.IntegrationObject{
		Path: "/widgets", Method: http.MethodPost,
		RequestContent: testRequestContent("application/merge-patch+json", models.RequestSerializationJSON),
	}
	status, err := dispatcher.ExecuteStream(
		context.Background(), service, explicitAnonymousEndpoint(endpoint), map[string]any{"enabled": true}, nil, nil, &mockStream{},
	)
	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if gotContentType != endpoint.RequestContent.Representations[0].MediaType {
		t.Fatalf("provider Content-Type = %q, want %q", gotContentType, endpoint.RequestContent.Representations[0].MediaType)
	}
}

func TestPrepareRequestParts_GraphQLWithVariables(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	query := `query Viewer($id: ID!) { viewer(id: $id) { login } }`
	obj := &models.IntegrationObject{
		Path:             "/graphql",
		Method:           http.MethodPost,
		GraphQLQuery:     &query,
		ProviderProtocol: models.ProviderProtocolGraphQL,
		OperationKind:    models.OperationKindQuery,
		RequestContent:   testRequestContent("application/json", models.RequestSerializationJSON),
	}
	params := map[string]any{"id": "u_123"}

	reqURL, headers, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}
	if reqURL != "https://api.example.com/graphql" {
		t.Errorf("unexpected URL: %s", reqURL)
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("expected application/json, got %s", headers["Content-Type"])
	}
	if bodyReader == nil {
		t.Fatalf("expected non-nil bodyReader")
	}
	var decoded map[string]any
	if err := json.NewDecoder(bodyReader).Decode(&decoded); err != nil {
		t.Fatalf("body was not valid JSON: %v", err)
	}
	if decoded["query"] != query {
		t.Errorf("query = %v, want %v", decoded["query"], query)
	}
	variables, ok := decoded["variables"].(map[string]any)
	if !ok {
		t.Fatalf("variables missing or wrong type: %#v", decoded["variables"])
	}
	if variables["id"] != "u_123" {
		t.Errorf("variables[id] = %v, want u_123", variables["id"])
	}
}

func TestPrepareRequestParts_GraphQLUnwrapsGeneratedSDKEnvelope(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	query := `query Viewer($id: ID!) { viewer(id: $id) { login } }`
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query, ProviderProtocol: models.ProviderProtocolGraphQL, OperationKind: models.OperationKindQuery, RequestContent: testRequestContent("application/json", models.RequestSerializationJSON)}
	params := map[string]any{
		"query":     `mutation DeleteAccount { deleteAccount }`,
		"variables": map[string]any{"id": "u_123"},
	}

	_, _, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}
	var decoded map[string]any
	if err := json.NewDecoder(bodyReader).Decode(&decoded); err != nil {
		t.Fatalf("body was not valid JSON: %v", err)
	}
	if decoded["query"] != query {
		t.Errorf("query = %v, want stored operation %v", decoded["query"], query)
	}
	variables, ok := decoded["variables"].(map[string]any)
	if !ok || variables["id"] != "u_123" {
		t.Fatalf("variables were not unwrapped: %#v", decoded["variables"])
	}
	if _, nested := variables["variables"]; nested {
		t.Fatalf("generated SDK envelope was nested again: %#v", variables)
	}
}

func TestGraphQLVariablesDoesNotUnwrapOrdinaryVariablesNamedQuery(t *testing.T) {
	params := map[string]any{"query": "customer search", "limit": 10}
	if got := graphQLVariables(params); !reflect.DeepEqual(got, params) {
		t.Fatalf("ordinary variables changed: %#v", got)
	}
}

func TestPrepareRequestParts_GraphQLOmitsVariablesWhenEmpty(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	query := `query Health { health }`
	obj := &models.IntegrationObject{
		Path:             "/graphql",
		Method:           http.MethodPost,
		GraphQLQuery:     &query,
		ProviderProtocol: models.ProviderProtocolGraphQL,
		OperationKind:    models.OperationKindQuery,
	}

	_, _, bodyReader, err := prepareRequestParts(srv, obj, nil, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}
	var decoded map[string]any
	if err := json.NewDecoder(bodyReader).Decode(&decoded); err != nil {
		t.Fatalf("body was not valid JSON: %v", err)
	}
	if _, present := decoded["variables"]; present {
		t.Errorf("expected no \"variables\" key for a query with no params, got %#v", decoded["variables"])
	}
	if decoded["query"] != query {
		t.Errorf("query = %v, want %v", decoded["query"], query)
	}
}

func TestPrepareRequestParts_GraphQLBypassesGetDeleteBodySuppression(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	query := `query Health { health }`
	obj := &models.IntegrationObject{
		Path:             "/graphql",
		Method:           http.MethodGet,
		GraphQLQuery:     &query,
		ProviderProtocol: models.ProviderProtocolGraphQL,
		OperationKind:    models.OperationKindQuery,
	}

	_, _, bodyReader, err := prepareRequestParts(srv, obj, nil, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}
	if bodyReader == nil {
		t.Fatalf("expected a GraphQL query body even for a GET-declared object")
	}
}

func TestBuildBaseURL(t *testing.T) {
	tests := []struct {
		reqURL   string
		baseURL  string
		expected string
	}{
		{"/path", "https://api.example.com", "https://api.example.com/path"},
		{"path", "https://api.example.com/", "https://api.example.com/path"},
		{"path", "https://api.example.com", "https://api.example.com/path"},
		{"/path", "https://api.example.com/", "https://api.example.com//path"}, // Note: double slash isn't stripped by this simple logic, but the current logic handles `path` + `baseURL` combinations
		{"https://other.com/path", "https://api.example.com", "https://other.com/path"},
	}
	for _, tt := range tests {
		result := buildBaseURL(tt.reqURL, tt.baseURL)
		if result != tt.expected {
			t.Errorf("buildBaseURL(%q, %q) = %q, want %q", tt.reqURL, tt.baseURL, result, tt.expected)
		}
	}
}

func TestPrepareRequestParts_NilRequestContentRejectsBodyInput(t *testing.T) {
	service := &models.Service{BaseURL: "https://api.example.com"}
	endpoint := &models.IntegrationObject{Path: "/widgets", Method: http.MethodPost}
	if _, _, _, err := prepareRequestParts(service, endpoint, map[string]any{"name": "ignored"}, nil); err == nil || !strings.Contains(err.Error(), `undeclared execution parameter "name"`) {
		t.Fatalf("nil request content error = %v", err)
	}
}

func TestBuildRESTRequestBodyJSONPreservesRootPayloadShape(t *testing.T) {
	root := testRequestContent("application/json", models.RequestSerializationJSON)
	root.PayloadParameter = "body"
	for name, test := range map[string]struct {
		value any
		want  string
	}{
		"scalar": {value: 7, want: "7"},
		"array":  {value: []any{1, 2}, want: "[1,2]"},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := buildRESTRequestBody(root, map[string]string{}, map[string]any{"body": test.value})
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := io.ReadAll(body)
			if err != nil || string(encoded) != test.want {
				t.Fatalf("root JSON body = %q, %v", encoded, err)
			}
		})
	}
	object := testRequestContent("application/json", models.RequestSerializationJSON)
	body, err := buildRESTRequestBody(object, map[string]string{}, map[string]any{"name": "one"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(body)
	if err != nil || string(encoded) != `{"name":"one"}` {
		t.Fatalf("object JSON body = %q, %v", encoded, err)
	}
}

func TestMultipartRetryReusesExecutionLocalBoundaryAndBytes(t *testing.T) {
	content := testRequestContent("multipart/form-data", models.RequestSerializationMultipart)
	content.Representations[0].Encoding = map[string]models.RequestEncoding{"name": {ContentType: "text/plain"}}
	params := map[string]any{"name": "replay-value"}
	ctx := withMultipartReplayBoundary(context.Background())
	firstType, firstBody := encodedMultipartAttempt(t, ctx, content, params)
	secondType, secondBody := encodedMultipartAttempt(t, ctx, content, params)
	if firstType != secondType || !bytes.Equal(firstBody, secondBody) {
		t.Fatalf("multipart retry changed bytes/content type: %q/%q", firstType, secondType)
	}
	otherType, _ := encodedMultipartAttempt(t, withMultipartReplayBoundary(context.Background()), content, params)
	if otherType == firstType {
		t.Fatal("independent executions reused a multipart boundary")
	}
}

func TestPositionalMultipartRetryReusesExecutionLocalBoundaryAndBytes(t *testing.T) {
	content := positionalTestContent("multipart/mixed", "items", "string", "", models.RequestEncoding{ContentType: "text/plain"})
	params := map[string]any{"items": []any{"first", "second"}}
	ctx := withMultipartReplayBoundary(context.Background())
	firstType, firstBody := encodedSelectedMultipartAttempt(t, ctx, content, params)
	secondType, secondBody := encodedSelectedMultipartAttempt(t, ctx, content, params)
	if firstType != secondType || !bytes.Equal(firstBody, secondBody) {
		t.Fatalf("positional multipart retry changed bytes/content type: %q/%q", firstType, secondType)
	}
}

func encodedMultipartAttempt(t *testing.T, ctx context.Context, content *models.RequestContent, params map[string]any) (string, []byte) {
	t.Helper()
	selected, _, err := SelectRequestContent(content)
	if err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{}
	body, err := buildMultipartRequestBodyWithContext(ctx, selected, headers, params)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return headers["Content-Type"], encoded
}

func encodedSelectedMultipartAttempt(t *testing.T, ctx context.Context, content *SelectedRequestRepresentation, params map[string]any) (string, []byte) {
	t.Helper()
	headers := map[string]string{}
	body, err := buildMultipartRequestBodyWithContext(ctx, content, headers, params)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return headers["Content-Type"], encoded
}

func TestPrepareRequestParts_RequestContentRequiresMediaType(t *testing.T) {
	endpoint := &models.IntegrationObject{
		Path: "/widgets", Method: http.MethodPost,
		RequestContent: testRequestContent("", models.RequestSerializationJSON),
	}
	_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{"name": "one"}, nil)
	if err == nil || !strings.Contains(err.Error(), "media_type") {
		t.Fatalf("missing media_type error = %v", err)
	}
}

func TestPrepareRequestParts_UnsupportedRequestSerializationFails(t *testing.T) {
	for _, serialization := range []string{"protobuf"} {
		t.Run(serialization, func(t *testing.T) {
			endpoint := &models.IntegrationObject{Path: "/widgets", Method: http.MethodPost, RequestContent: testRequestContent("application/octet-stream", serialization)}
			_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{"name": "one"}, nil)
			if err == nil || !strings.Contains(err.Error(), "serialization") {
				t.Fatalf("serialization %q error = %v", serialization, err)
			}
		})
	}
}

type capturedMultipartPart struct {
	contentType string
	payload     []byte
}

func readMultipartParts(t *testing.T, contentType string, body io.Reader) map[string]capturedMultipartPart {
	t.Helper()
	_, parameters, err := mime.ParseMediaType(contentType)
	if err != nil || parameters["boundary"] == "" {
		t.Fatalf("multipart Content-Type %q has no valid boundary: %v", contentType, err)
	}
	reader := multipart.NewReader(body, parameters["boundary"])
	parts := make(map[string]capturedMultipartPart)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatalf("read multipart part: %v", err)
		}
		payload, err := io.ReadAll(part)
		if err != nil {
			t.Fatalf("read multipart payload: %v", err)
		}
		parts[part.FormName()] = capturedMultipartPart{contentType: part.Header.Get("Content-Type"), payload: payload}
	}
}

func TestPrepareRequestParts_MultipartUsesReviewedPartEncoding(t *testing.T) {
	endpoint := &models.IntegrationObject{
		Path: "/uploads", Method: http.MethodPost,
		RequestContent: &models.RequestContent{
			Representations: []models.RequestRepresentation{{MediaType: "multipart/form-data", Serialization: models.RequestSerializationMultipart, Encoding: map[string]models.RequestEncoding{
				"file": {ContentType: "image/png", BinaryEncoding: models.RequestBinaryEncodingBase64},
				"blob": {BinaryEncoding: models.RequestBinaryEncodingBase64},
			}, Schema: &models.SchemaContract{Projection: models.Schema{Type: "object", AdditionalProperties: &models.Schema{}}}}},
		},
	}
	_, headers, body, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{
		"name": "widget", "count": 3, "metadata": map[string]any{"active": true},
		"file": "aGVsbG8=", "blob": "AAE=",
	}, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts: %v", err)
	}
	parts := readMultipartParts(t, headers["Content-Type"], body)
	if string(parts["name"].payload) != "widget" || string(parts["count"].payload) != "3" {
		t.Fatalf("scalar multipart parts = %#v", parts)
	}
	if string(parts["metadata"].payload) != `{"active":true}` || parts["metadata"].contentType != "application/json" {
		t.Fatalf("composite multipart part = %#v", parts["metadata"])
	}
	if string(parts["file"].payload) != "hello" || parts["file"].contentType != "image/png" {
		t.Fatalf("configured binary multipart part = %#v", parts["file"])
	}
	if !bytes.Equal(parts["blob"].payload, []byte{0, 1}) || parts["blob"].contentType != "application/octet-stream" {
		t.Fatalf("default binary multipart part = %#v", parts["blob"])
	}
}

func TestPrepareRequestParts_MultipartRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name        string
		part        models.RequestEncoding
		value       any
		wantMessage string
	}{
		{name: "base64 type", part: models.RequestEncoding{BinaryEncoding: "base64"}, value: 42, wantMessage: `requires a base64 string`},
		{name: "invalid base64", part: models.RequestEncoding{BinaryEncoding: "base64"}, value: "not-base64", wantMessage: `contains invalid base64`},
		{name: "unsupported encoding", part: models.RequestEncoding{BinaryEncoding: "hex"}, value: "00", wantMessage: `unsupported binary_encoding`},
		{name: "invalid content type", part: models.RequestEncoding{ContentType: "bad\r\nInjected: yes"}, value: "value", wantMessage: `invalid content_type`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := &models.IntegrationObject{Path: "/uploads", Method: http.MethodPost, RequestContent: &models.RequestContent{
				Representations: []models.RequestRepresentation{{MediaType: "multipart/form-data", Serialization: models.RequestSerializationMultipart, Encoding: map[string]models.RequestEncoding{"file": tt.part}}},
			}}
			_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{"file": tt.value}, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("multipart error = %v, want %q", err, tt.wantMessage)
			}
		})
	}
}

func TestPrepareRequestParts_RawUsesConfiguredPayload(t *testing.T) {
	tests := []struct {
		name           string
		value          any
		binaryEncoding string
		want           []byte
	}{
		{name: "string", value: "plain text", want: []byte("plain text")},
		{name: "bytes", value: []byte{0, 1}, want: []byte{0, 1}},
		{name: "base64", value: "aGVsbG8=", binaryEncoding: models.RequestBinaryEncodingBase64, want: []byte("hello")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := models.Schema{Type: "string"}
			if tt.binaryEncoding == models.RequestBinaryEncodingBase64 {
				schema.Format = "binary"
			}
			content := testRequestContentWithSchema("application/octet-stream", models.RequestSerializationRaw, schema)
			content.PayloadParameter = "body"
			endpoint := &models.IntegrationObject{Path: "/raw", Method: http.MethodPost, RequestContent: content}
			_, _, body, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{"body": tt.value}, nil)
			if err != nil {
				t.Fatalf("prepareRequestParts: %v", err)
			}
			payload, err := io.ReadAll(body)
			if err != nil || !bytes.Equal(payload, tt.want) {
				t.Fatalf("raw payload = %q, err=%v, want %q", payload, err, tt.want)
			}
		})
	}
}

func TestPrepareRequestParts_RawRejectsInvalidPayload(t *testing.T) {
	tests := []struct {
		name        string
		content     *models.RequestContent
		params      map[string]any
		wantMessage string
	}{
		{name: "missing convention", content: testRequestContent("text/plain", models.RequestSerializationRaw), params: map[string]any{"body": "x"}, wantMessage: `payload_parameter is required`},
		{name: "missing payload", content: rawTestRequestContent("text/plain", false), params: map[string]any{}, wantMessage: `missing raw request payload parameter "body"`},
		{name: "ambiguous extras", content: rawTestRequestContent("text/plain", false), params: map[string]any{"body": "value", "extra": "lossy"}, wantMessage: `undeclared execution parameter "extra"`},
		{name: "wrong type", content: rawTestRequestContent("text/plain", false), params: map[string]any{"body": map[string]any{"not": "JSON"}}, wantMessage: `must be a string or byte array`},
		{name: "invalid base64", content: rawTestRequestContent("application/octet-stream", true), params: map[string]any{"body": "not-base64"}, wantMessage: `contains invalid base64`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := &models.IntegrationObject{Path: "/raw", Method: http.MethodPost, RequestContent: tt.content}
			_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, tt.params, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantMessage) {
				t.Fatalf("raw error = %v, want %q", err, tt.wantMessage)
			}
		})
	}
}

func TestPrepareRequestParts_RequiredRequestContentRejectsEmptyBody(t *testing.T) {
	for _, serialization := range []string{models.RequestSerializationJSON, models.RequestSerializationForm, models.RequestSerializationMultipart} {
		t.Run(serialization, func(t *testing.T) {
			mediaType := "application/json"
			if serialization == models.RequestSerializationForm {
				mediaType = "application/x-www-form-urlencoded"
			}
			if serialization == models.RequestSerializationMultipart {
				mediaType = "multipart/form-data"
			}
			content := testRequestContent(mediaType, serialization)
			content.Required = true
			endpoint := &models.IntegrationObject{Path: "/required", Method: http.MethodPost, RequestContent: content}
			_, _, _, err := prepareRequestParts(&models.Service{BaseURL: "https://api.example.com"}, endpoint, map[string]any{}, nil)
			if err == nil || !strings.Contains(err.Error(), "missing required request body") {
				t.Fatalf("required %s body error = %v", serialization, err)
			}
		})
	}
}
