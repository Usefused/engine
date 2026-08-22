package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
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
