package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		status:      http.StatusBadRequest,
		code:        "bucket_credentials_missing",
		message:     "The selected credential set is missing required authentication material.",
		category:    "validation",
		details:     map[string]any{"missing": []string{"service-id (basic:jira_password)"}},
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
	if response.Error.Code != "bucket_credentials_missing" || response.Error.Category != "validation" || response.Error.Retryable {
		t.Fatalf("unexpected response metadata: %#v", response)
	}
	if response.Error.TraceID != traceID.String() {
		t.Fatalf("trace_id = %q, want %q", response.Error.TraceID, traceID.String())
	}
	if response.Error.Remediation == "" || response.Error.Details == nil {
		t.Fatalf("expected actionable details, got %#v", response)
	}
}

func TestWriteSDKConfigErrorDoesNotForwardRegistryBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeSDKConfigError(recorder, sdkProxyError{
		status: http.StatusServiceUnavailable,
		body:   []byte(`{"error":"Authorization: Bearer secret-value"}`),
	})

	if strings.Contains(recorder.Body.String(), "secret-value") {
		t.Fatalf("response exposed Registry body: %s", recorder.Body.String())
	}
	var response workspaceConfigErrorResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Error.Code != "registry_request_failed" || response.Error.Category != "dependency" || !response.Error.Retryable {
		t.Fatalf("unexpected response metadata: %#v", response)
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
