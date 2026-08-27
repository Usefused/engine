package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestMCPMessageLimitTelemetryIgnoresOtherReadErrors prevents raw reader errors from entering OTEL.
func TestMCPMessageLimitTelemetryIgnoresOtherReadErrors(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	recordMCPMessageLimit(context.Background(), "app", "sse", errors.New("secret reader failure"))
	// Ordinary body failures must not masquerade as hard-limit rejections.
	if len(exporter.GetSpans()) != 0 {
		t.Fatal("ordinary read failure created a limit span")
	}
	recordMCPMessageLimit(context.Background(), "app", "sse", &http.MaxBytesError{Limit: maxMCPMessageBodyBytes})
	spans := exporter.GetSpans()
	// The typed oversized-body result produces exactly the compiled policy code.
	if len(spans) != 1 || spans[0].Status.Description != "mcp_message_payload_too_large" {
		t.Fatalf("unexpected payload spans: %#v", spans)
	}
}

// TestMCPRuntimeOutputLimitTelemetryAllowlist checks admission and delivery policies without inspecting result values.
func TestMCPRuntimeOutputLimitTelemetryAllowlist(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess := &mcpSession{appID: "app-safe", transport: "sse", token: "secret-token"}
	for _, code := range []string{"MCP_DOCUMENTATION_OUTPUT_LIMIT_EXCEEDED", "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED", "MCP_EXECUTE_ERROR_OUTPUT_LIMIT_EXCEEDED", "MCP_EXECUTE_VISIBLE_OUTPUT_LIMIT_EXCEEDED"} {
		recordMCPRuntimeOutputLimit(context.Background(), sess, mcpLimitTestEnvelope(t, code, true))
	}
	recordMCPRuntimeOutputLimit(context.Background(), sess, mcpLimitTestEnvelope(t, "secret-error", true))
	recordMCPRuntimeOutputLimit(context.Background(), sess, mcpLimitTestEnvelope(t, "MCP_EXECUTE_RESULT_LIMIT_EXCEEDED", false))
	spans := exporter.GetSpans()
	// Only the four fixed policy failures may cross the telemetry boundary.
	if len(spans) != 4 {
		t.Fatalf("limit span count = %d, want 4", len(spans))
	}
	for _, span := range spans {
		attributes := mcpSearchTestAttributes(span)
		// Fixed dimensions forbid both arbitrary child fields and observed payload sizes.
		if len(attributes) != 6 || attributes["app.id"] != "app-safe" || attributes["outcome"] != "rejected" {
			t.Fatalf("unexpected limit attributes: %#v", attributes)
		}
		// The child failure's extra fields must never escape the fixed code projection.
		if strings.Contains(strings.Join(mcpSearchTestValues(attributes), " "), "secret") {
			t.Fatalf("secret escaped limit telemetry: %#v", attributes)
		}
	}
}

// mcpLimitTestEnvelope supplies secret-bearing child error fields to test the narrow code projection.
func mcpLimitTestEnvelope(t *testing.T, code string, isError bool) string {
	t.Helper()
	content, err := json.Marshal(map[string]any{"code": code, "message": "secret-content", "actual_bytes": 99})
	// Test data must itself be valid JSON before it exercises the runtime projection.
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(map[string]any{"result": map[string]any{
		"isError": isError, "content": []map[string]string{{"type": "text", "text": string(content)}},
	}})
	// Encoding failure would invalidate this test rather than prove safe projection.
	if err != nil {
		t.Fatal(err)
	}
	return string(envelope)
}

// TestMCPSessionStartFailurePreservesOnlyTypedSchemaCode makes admission recovery observable and secret-safe.
func TestMCPSessionStartFailurePreservesOnlyTypedSchemaCode(t *testing.T) {
	status, code := mcpSessionStartFailure(fmt.Errorf("secret-schema: %w", ErrMCPSchemaDepthLimit))
	// Wrapped schema context must not conceal the stable policy code or leak its source.
	if status != http.StatusUnprocessableEntity || code != mcpSchemaDepthLimitCode {
		t.Fatalf("schema start failure = %d/%q", status, code)
	}
	status, code = mcpSessionStartFailure(errors.New("secret-runtime-path"))
	// Unknown startup failures retain one stable code without raw process details.
	if status != http.StatusInternalServerError || code != mcpSessionStartFailedCode {
		t.Fatalf("runtime start failure = %d/%q", status, code)
	}
}

// TestMCPSessionStartFailureDataSeparatesAgentActionFromTelemetry guards the compact public contract.
func TestMCPSessionStartFailureDataSeparatesAgentActionFromTelemetry(t *testing.T) {
	for _, test := range []struct {
		code, recovery string
	}{
		{code: mcpSessionStartFailedCode, recovery: "retry_initialize"},
		{code: mcpSchemaDepthLimitCode, recovery: "contact_server_owner"},
	} {
		failure := mcpSessionStartFailureData(test.code)
		encoded, err := json.Marshal(failure)
		// Serialization must succeed before the field allowlist can prove what an agent receives.
		if err != nil {
			t.Fatal(err)
		}
		var public map[string]any
		// A malformed recovery envelope would be less actionable than the generic response it replaces.
		if json.Unmarshal(encoded, &public) != nil {
			t.Fatalf("invalid recovery JSON: %s", encoded)
		}
		if failure.RecoveryAction != test.recovery || failure.ExecuteRequest != "unchanged" || failure.ProviderExecution != "not_started" {
			t.Fatalf("failure = %+v, want recovery %q with unchanged execute", failure, test.recovery)
		}
		for _, hidden := range []string{"phase", "session_state", "request_delivery", "side_effect_state"} {
			// Internal classification belongs on bounded OTEL attributes, not in model-visible recovery data.
			if _, exposed := public[hidden]; exposed {
				t.Fatalf("recovery exposed internal field %q: %s", hidden, encoded)
			}
		}
	}
}
