package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestWriteWorkspaceConfigErrorReturnsStructuredEngineError(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("0123456789abcdef0123456789abcdef")
	spanID, _ := trace.SpanIDFromHex("0123456789abcdef")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))
	recorder := httptest.NewRecorder()

	writeWorkspaceConfigError(recorder, workspaceConfigHTTPError{
		status:   http.StatusBadRequest,
		code:     "bucket_credentials_missing",
		message:  "The selected credential set is missing required authentication material.",
		category: "validation",
		details: map[string]any{
			"bucket": appReadinessBucket{ID: "4d7fbc58-c058-4e9a-9da0-745719303424", Name: "production"},
			"missing_credentials": []appMissingCredential{{
				ServiceID: "18cfc8d2-2f4c-47b2-8e2c-ae7ad54b799d", Service: "Jira",
				AuthType: "bearer", AuthName: "bearerAuth", RequiredFields: []appMissingCredentialField{{Name: "token", SecretKey: "bearerAuth"}},
			}},
		},
		remediation: "Add the required credentials and create the plan again.",
	}, ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if contentType := recorder.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", contentType)
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	assertWorkspaceConfigErrorEnvelope(t, response, traceID.String())
	assertCredentialReadinessWireDetails(t, recorder.Body.Bytes())
}

// TestWriteWorkspaceConfigErrorPreservesMutationProof verifies that the shared
// typed error can carry every existing recovery and correlation envelope field.
func TestWriteWorkspaceConfigErrorPreservesMutationProof(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeWorkspaceConfigError(recorder, workspaceConfigHTTPError{
		status: http.StatusConflict, code: "config_apply_failed", message: "The configuration could not be applied.", category: "conflict",
		phase: "workspace_commit", operationID: "plan-123", requestID: "request-123", commitState: "not_committed",
		recovery: "fused-cli workspace apply workspace.yaml", traceID: "0123456789abcdef0123456789abcdef",
	})

	var response workspaceConfigErrorResponse
	// The wire response must decode through the same public envelope used by CLI callers.
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.Phase != "workspace_commit" || response.Error.OperationID != "plan-123" || response.Error.RequestID != "request-123" {
		t.Fatalf("mutation identity metadata = %#v", response.Error)
	}
	if response.Error.CommitState != "not_committed" || response.Error.Recovery != "fused-cli workspace apply workspace.yaml" || response.Error.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("mutation recovery metadata = %#v", response.Error)
	}
}

// TestWriteWorkspaceConfigErrorDisablesCommittedMutationRetry prevents a
// response-projection failure from asking automation to repeat committed work.
func TestWriteWorkspaceConfigErrorDisablesCommittedMutationRetry(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeWorkspaceConfigError(recorder, workspaceConfigHTTPError{
		status: http.StatusInternalServerError, code: "response_projection_failed",
		message: "The committed result could not be projected.", phase: "response_projection", commitState: "committed",
	})

	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// A proven commit is never an automatic retry and must tell the caller to inspect state.
	if response.Error.Retryable || response.Error.CommitState != "committed" || response.Error.Remediation == "" {
		t.Fatalf("committed error envelope = %#v", response.Error)
	}
}

// TestWriteSDKConfigAuthorizationErrorPreservesMutationProof keeps actionable
// missing permissions together with the plan apply's pre-commit evidence.
func TestWriteSDKConfigAuthorizationErrorPreservesMutationProof(t *testing.T) {
	resource := accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: uuid.New()}
	denied := &accesscontrol.PermissionDeniedError{Missing: []accesscontrol.Requirement{{Permission: accesscontrol.PermissionAppManage, Resource: resource}}}
	recorder := httptest.NewRecorder()
	writeSDKConfigError(recorder, withWorkspaceConfigErrorMetadata(denied, "apply_admission", "plan-authorization", "not_committed"))

	var response struct {
		Error struct {
			Phase       string `json:"phase"`
			OperationID string `json:"operation_id"`
			CommitState string `json:"commit_state"`
			Details     struct {
				Missing []map[string]any `json:"missing"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// The denial must not lose either its recovery state or typed permission requirement.
	if response.Error.Phase != "apply_admission" || response.Error.OperationID != "plan-authorization" || response.Error.CommitState != "not_committed" || len(response.Error.Details.Missing) != 1 {
		t.Fatalf("authorization mutation envelope = %#v", response.Error)
	}
}

// TestWriteWorkspaceConfigErrorRecordsSafeMutationTelemetry proves the shared
// error boundary annotates its existing span without exporting internal prose.
func TestWriteWorkspaceConfigErrorRecordsSafeMutationTelemetry(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "engine.test.mutation")
	secret := "database password=fsk_never_trace"
	response := httptest.NewRecorder()

	writeWorkspaceConfigError(response, workspaceConfigHTTPError{
		status: http.StatusInternalServerError, code: "bucket_value_save_failed",
		message: "The Engine could not save the bucket value.", phase: "bucket_value_upsert", commitState: "unknown",
		cause: errors.New(secret),
	}, ctx)
	span.End()

	ended := recorder.Ended()
	// One request span remains the sole telemetry owner for the mutation failure.
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	assertSafeControlMutationTelemetry(t, ended[0], secret)
}

// assertSafeControlMutationTelemetry checks the exact stable mutation fields
// and keeps secret-denylist branching out of the lifecycle test itself.
func assertSafeControlMutationTelemetry(t *testing.T, span sdktrace.ReadOnlySpan, secret string) {
	t.Helper()
	attributes := map[string]string{}
	for _, item := range span.Attributes() {
		attributes[string(item.Key)] = item.Value.AsString()
	}
	want := map[string]string{
		"control.mutation.outcome":      "failed",
		"control.mutation.error_code":   "bucket_value_save_failed",
		"control.mutation.phase":        "bucket_value_upsert",
		"control.mutation.commit_state": "unknown",
	}
	// Only stable HTTP-admitted mutation metadata is copied onto the span.
	for key, value := range want {
		// Every fixed field must match independently of exporter attribute order.
		if attributes[key] != value {
			t.Fatalf("mutation telemetry[%q] = %q, want %q: %#v", key, attributes[key], value, attributes)
		}
	}
	// Error status uses the same stable classifier rather than the underlying cause.
	if span.Status().Code != codes.Error || span.Status().Description != "bucket_value_save_failed" {
		t.Fatalf("mutation span status = %#v", span.Status())
	}
	encoded, _ := json.Marshal(attributes)
	// Database prose and credentials must remain absent from exported attributes.
	for _, forbidden := range []string{secret, "fsk_never_trace", "password"} {
		// Each marker independently guards against partial redaction regressions.
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("mutation telemetry leaked internal cause %q: %s", forbidden, encoded)
		}
	}
}

// TestWriteWorkspaceConfigErrorUsesRequestCorrelation verifies that generic
// failures retain middleware correlation while suppressing raw secret prose.
func TestWriteWorkspaceConfigErrorUsesRequestCorrelation(t *testing.T) {
	handler := chimiddleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeWorkspaceConfigError(w, errors.New("database secret=fsk_never_return"), r.Context())
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/workspace/config/apply", nil))

	var response workspaceConfigErrorResponse
	// The fallback must remain structured even though its source error is untyped.
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.RequestID == "" || response.Error.Code != "workspace_config_error" {
		t.Fatalf("generic correlation metadata = %#v", response.Error)
	}
	// Neither the secret value nor its identifying raw-error marker is public.
	if strings.Contains(recorder.Body.String(), "fsk_never_return") || strings.Contains(recorder.Body.String(), "database secret") {
		t.Fatalf("generic response exposed raw error: %s", recorder.Body.String())
	}
}

// TestWriteWorkspaceConfigErrorFallbackIncludesTraceID verifies that generic
// internal prose still carries the correlation identity needed for log lookup.
func TestWriteWorkspaceConfigErrorFallbackIncludesTraceID(t *testing.T) {
	traceID, _ := trace.TraceIDFromHex("fedcba9876543210fedcba9876543210")
	spanID, _ := trace.SpanIDFromHex("fedcba9876543210")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID,
		SpanID:  spanID,
	}))
	recorder := httptest.NewRecorder()

	writeWorkspaceConfigError(recorder, errors.New("database password=fsk_never_return"), ctx)

	// Unknown errors retain the stable internal-server status.
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	var response workspaceConfigErrorResponse
	// The fallback remains valid structured JSON for CLI consumers.
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	// Correlation and retry metadata must survive the generic fallback boundary.
	if response.Error.TraceID != traceID.String() || response.Error.Code != "workspace_config_error" || !response.Error.Retryable {
		t.Fatalf("fallback envelope = %#v", response.Error)
	}
	// Unknown internal prose may contain secrets, so the generic response must
	// retain correlation identity without forwarding the underlying error.
	if strings.Contains(recorder.Body.String(), "fsk_never_return") || strings.Contains(recorder.Body.String(), "password") {
		t.Fatalf("fallback response exposed internal error: %s", recorder.Body.String())
	}
}

func assertWorkspaceConfigErrorEnvelope(t *testing.T, response workspaceConfigErrorResponse, traceID string) {
	t.Helper()
	if response.Error.Code != "bucket_credentials_missing" || response.Error.Category != "validation" || response.Error.Retryable {
		t.Fatalf("unexpected response metadata: %#v", response)
	}
	if response.Error.TraceID != traceID {
		t.Fatalf("trace_id = %q, want %q", response.Error.TraceID, traceID)
	}
	if response.Error.Remediation == "" || response.Error.Details == nil {
		t.Fatalf("expected actionable details, got %#v", response)
	}
}

func assertCredentialReadinessWireDetails(t *testing.T, body []byte) {
	t.Helper()
	var wire struct {
		Error struct {
			Details struct {
				Bucket             appReadinessBucket     `json:"bucket"`
				MissingCredentials []appMissingCredential `json:"missing_credentials"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatalf("decode typed readiness details: %v", err)
	}
	wantBucket := appReadinessBucket{ID: "4d7fbc58-c058-4e9a-9da0-745719303424", Name: "production"}
	if wire.Error.Details.Bucket != wantBucket {
		t.Fatalf("bucket details = %#v", wire.Error.Details.Bucket)
	}
	wantMissing := []appMissingCredential{{
		ServiceID: "18cfc8d2-2f4c-47b2-8e2c-ae7ad54b799d", Service: "Jira",
		AuthType: "bearer", AuthName: "bearerAuth", RequiredFields: []appMissingCredentialField{{Name: "token", SecretKey: "bearerAuth"}},
	}}
	if !reflect.DeepEqual(wire.Error.Details.MissingCredentials, wantMissing) {
		t.Fatalf("missing credentials = %#v", wire.Error.Details.MissingCredentials)
	}
	raw := string(body)
	for _, legacy := range []string{`"missing":`, `"service_name":`, `"secret_keys":`} {
		if strings.Contains(raw, legacy) {
			t.Fatalf("legacy readiness field %s leaked into response: %s", legacy, raw)
		}
	}
}

// TestWriteSDKConfigErrorDoesNotForwardRegistryBody verifies proxy recovery
// metadata survives without forwarding the untrusted Registry error body.
func TestWriteSDKConfigErrorDoesNotForwardRegistryBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSDKConfigError(recorder, withWorkspaceConfigErrorMetadata(sdkProxyError{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"error":"Authorization: Bearer secret-value"}`),
	}, "registry_generation", "plan-456", "unknown"))

	if strings.Contains(recorder.Body.String(), "secret-value") {
		t.Fatalf("response exposed Registry body: %s", recorder.Body.String())
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.Code != "registry_request_failed" || response.Error.Category != "dependency" || response.Error.Retryable {
		t.Fatalf("unexpected response metadata: %#v", response)
	}
	if response.Error.Phase != "registry_generation" || response.Error.OperationID != "plan-456" || response.Error.CommitState != "unknown" {
		t.Fatalf("proxy mutation metadata = %#v", response.Error)
	}
}

func TestOneTimeSecretResponseHeadersDisableCaching(t *testing.T) {
	recorder := httptest.NewRecorder()
	setOneTimeSecretResponseHeaders(recorder)

	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := recorder.Header().Get("Pragma"); got != "no-cache" {
		t.Fatalf("Pragma = %q, want no-cache", got)
	}
}
