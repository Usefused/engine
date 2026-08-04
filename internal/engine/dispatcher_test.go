package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type mockStream struct {
	chunks [][]byte
}

func TestPrepareRequestPartsBindingPrecedence(t *testing.T) {
	srv := &models.Service{BaseURL: "https://service.example.com"}
	obj := &models.IntegrationObject{
		Path: "/accounts/{accountId}", Method: http.MethodGet,
		Parameters: models.Parameters{
			{Name: "accountId", In: "path"}, {Name: "portal", In: "query"}, {Name: "X-Tenant", In: "header"},
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
	copied := make([]byte, len(chunk))
	copy(copied, chunk)
	m.chunks = append(m.chunks, copied)
	return nil
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

	status, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

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
		&models.IntegrationObject{Path: "/test", Method: http.MethodGet},
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

	applyAuth(req, auths, map[string]any{"oidc_token": "id-token"})

	if got := req.Header.Get("Authorization"); got != "Bearer id-token" {
		t.Fatalf("Authorization = %q, want bearer token", got)
	}
}

func TestApplyHTTPBasicAuthRequiresUsernameAndPassword(t *testing.T) {
	auths := models.AuthConfigs{{Type: "http", Scheme: "basic", Name: "basicAuth"}}
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
			applyAuth(req, auths, tt.credentials)
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
		if _, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, &mockStream{}); err != nil {
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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 2}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 2}}
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query}
	params := map[string]any{"id": "u_123"}

	ctx := ContextWithIdempotencyKeyPresent(context.Background(), true)
	stream := &mockStream{}
	status, err := d.ExecuteStream(ctx, srv, obj, params, nil, nil, stream)
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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 2}}
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query}

	stream := &mockStream{}
	_, err := d.ExecuteStream(context.Background(), srv, obj, map[string]any{"name": "widget"}, nil, nil, stream)
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

	_, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, &mockStream{})

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
	if _, err := NewDispatcher().ExecuteStream(context.Background(), &models.Service{Name: "test", BaseURL: server.URL}, obj, nil, nil, nil, &mockStream{}); err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}

	for _, span := range recorder.Ended() {
		if span.Name() != "engine.dispatch.vendor_call" {
			continue
		}
		attributes := map[string]string{}
		for _, attr := range span.Attributes() {
			attributes[string(attr.Key)] = attr.Value.AsString()
			if strings.Contains(string(attr.Key), "query") || strings.Contains(string(attr.Key), "variables") {
				t.Fatalf("sensitive GraphQL payload attribute recorded: %s", attr.Key)
			}
		}
		if attributes["provider.protocol"] != "graphql" || attributes["graphql.operation.kind"] != "query" {
			t.Fatalf("unexpected protocol attributes: %#v", attributes)
		}
		return
	}
	t.Fatal("provider call span was not recorded")
}

// TestDispatcherExecuteStream_NoRetryWithoutConfig is the strict half of the
// "no hardcoded retries" rule: a service with no RetryConfig and no
// RetryOverride in context gets exactly one attempt, even on a 500.
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
	_, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

	if err == nil {
		t.Fatal("expected an error from the single failed attempt")
	}
	if attempts != 1 {
		t.Errorf("expected exactly 1 attempt with no retry config, got %d", attempts)
	}
}

// TestDispatcherExecuteStream_SDKOverrideWithoutServiceConfig verifies that a
// caller-supplied RetryOverride is honored when the service has no
// RetryConfig of its own, clamped to the hard SDK ceiling.
func TestDispatcherExecuteStream_SDKOverrideWithoutServiceConfig(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	maxRetries := 2
	ctx := ContextWithRetryOverride(context.Background(), &RetryOverride{MaxRetries: &maxRetries})
	stream := &mockStream{}
	status, err := d.ExecuteStream(ctx, srv, obj, nil, nil, nil, stream)

	if err != nil {
		t.Fatalf("ExecuteStream failed: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("expected status 200, got %d", status)
	}
	if attempts != 3 {
		t.Errorf("expected 3 attempts (1 + 2 retries), got %d", attempts)
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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 3}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodPost}

	stream := &mockStream{}
	_, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 2}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodPost}

	ctx := ContextWithIdempotencyKeyPresent(context.Background(), true)
	stream := &mockStream{}
	status, err := d.ExecuteStream(ctx, srv, obj, nil, nil, nil, stream)

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
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 2}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	stream := &mockStream{}
	status, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

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

// TestDispatcherExecuteStream_SDKOverrideClampedToServiceCeiling verifies the
// override can only narrow a service's own RetryConfig, never widen it: the
// SDK asks for more retries than the service allows, and gets the service's
// ceiling instead.
func TestDispatcherExecuteStream_SDKOverrideClampedToServiceCeiling(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL, RetryConfig: &models.RetryConfig{MaxRetries: 1}}
	obj := &models.IntegrationObject{Path: "/test", Method: http.MethodGet}

	requested := 10
	ctx := ContextWithRetryOverride(context.Background(), &RetryOverride{MaxRetries: &requested})
	stream := &mockStream{}
	_, _ = d.ExecuteStream(ctx, srv, obj, nil, nil, nil, stream)

	if attempts != 2 {
		t.Errorf("expected attempts clamped to service ceiling (1 retry = 2 attempts), got %d", attempts)
	}
}

func TestDispatcherExecuteStream_Paginated(t *testing.T) {
	pagesServed := 0
	// Mock a server that serves exactly 2 pages using cursor parameter.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pagesServed++
		cursor := r.URL.Query().Get("cursor")
		w.WriteHeader(http.StatusOK)

		if cursor == "" {
			w.Write([]byte(`{"data":["item1"],"meta":{"next_cursor":"page2"}}`))
		} else if cursor == "page2" {
			w.Write([]byte(`{"data":["item2"],"meta":{"next_cursor":""}}`))
		} else {
			t.Errorf("Unexpected cursor: %s", cursor)
		}
	}))
	defer server.Close()

	d := NewDispatcher()
	srv := &models.Service{BaseURL: server.URL}
	obj := &models.IntegrationObject{
		Path:   "/test",
		Method: http.MethodGet,
		Pagination: &models.PaginationConfig{
			Type:         "cursor",
			RequestParam: "cursor",
			ResponsePath: "meta.next_cursor",
		},
	}
	stream := &mockStream{}

	status, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)

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

	if err := parseSSEEvents(strings.NewReader(raw), stream); err != nil {
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

// TestDispatcherExecuteStream_SSE validates full SSE routing through
// ExecuteStream when obj.IsSSE is true: the Engine sends the correct
// Accept header and each parsed event is forwarded as a separate chunk.
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
		IsSSE:  true,
	}
	stream := &mockStream{}

	status, err := d.ExecuteStream(context.Background(), srv, obj, nil, nil, nil, stream)
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
		"extra_query":     "bar", // fallback to query for GET
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
	if !strings.Contains(reqURL, "extra_query=bar") {
		t.Errorf("Fallback query parameter 'extra_query' missing. Got: %s", reqURL)
	}
	if headers["X-Custom-Header"] != "foo" {
		t.Errorf("Header parameter missing or incorrect. Got: %s", headers["X-Custom-Header"])
	}
	if bodyReader != nil {
		t.Errorf("Expected nil bodyReader for GET request, got non-nil")
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

func TestPrepareRequestParts_PathParameterLeaks(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/repos/{owner}/{repo}/issues",
		Method: http.MethodPost,
		// Explicitly simulate missing definitions in obj.Parameters
		Parameters: []models.Parameter{},
	}

	params := map[string]any{
		"Owner": "test-owner", // Different casing than URL
		"Repo":  "test-repo",  // Different casing than URL
		"title": "Bug report",
	}

	reqURL, _, bodyReader, err := prepareRequestParts(srv, obj, params, nil)
	if err != nil {
		t.Fatalf("prepareRequestParts failed: %v", err)
	}

	if reqURL != "https://api.example.com/repos/test-owner/test-repo/issues" {
		t.Errorf("Path parameters not correctly replaced or case-insensitive match failed. Got: %s", reqURL)
	}

	buf := new(bytes.Buffer)
	buf.ReadFrom(bodyReader)
	bodyStr := buf.String()

	if strings.Contains(bodyStr, "test-owner") || strings.Contains(bodyStr, "test-repo") {
		t.Errorf("Path parameters leaked into JSON payload! Got body: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, "title") {
		t.Errorf("Expected parameter 'title' in body, got: %s", bodyStr)
	}
}

func TestPrepareRequestParts_HeaderInjection(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	obj := &models.IntegrationObject{
		Path:   "/test",
		Method: http.MethodGet,
	}

	params := map[string]any{
		"param1": "value1",
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
	// This object requires form encoding but the service doesn't have it by default.
	obj := &models.IntegrationObject{
		Path:     "/test",
		Method:   http.MethodPost,
		Encoding: "application/x-www-form-urlencoded",
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

func TestPrepareRequestParts_GraphQLWithVariables(t *testing.T) {
	srv := &models.Service{BaseURL: "https://api.example.com"}
	query := `query Viewer($id: ID!) { viewer(id: $id) { login } }`
	obj := &models.IntegrationObject{
		Path:         "/graphql",
		Method:       http.MethodPost,
		GraphQLQuery: &query,
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
	obj := &models.IntegrationObject{Path: "/graphql", Method: http.MethodPost, GraphQLQuery: &query}
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
		Path:         "/graphql",
		Method:       http.MethodPost,
		GraphQLQuery: &query,
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
		Path:         "/graphql",
		Method:       http.MethodGet,
		GraphQLQuery: &query,
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

func TestResolveContentType(t *testing.T) {
	srv := &models.Service{
		DefaultHeaders: map[string]string{"Content-Type": "application/xml"},
	}
	obj := &models.IntegrationObject{Encoding: "text/plain"}
	headers := map[string]string{"content-type": "text/html"}

	// Priority 1: User injected header
	if ct := resolveContentType(obj, srv, headers); ct != "text/html" {
		t.Errorf("Expected text/html, got %s", ct)
	}

	// Priority 2: Service default
	delete(headers, "content-type")
	if ct := resolveContentType(obj, srv, headers); ct != "application/xml" {
		t.Errorf("Expected application/xml, got %s", ct)
	}

	// Priority 3: Endpoint encoding
	srv.DefaultHeaders = map[string]string{}
	if ct := resolveContentType(obj, srv, headers); ct != "text/plain" {
		t.Errorf("Expected text/plain, got %s", ct)
	}

	// Fallback
	obj.Encoding = ""
	if ct := resolveContentType(obj, srv, headers); ct != "application/json" {
		t.Errorf("Expected application/json, got %s", ct)
	}
}
