package sandbox

import (
	"reflect"
	"testing"
)

// TestMCPResultDeliveryAuditedOnce proves both completion races and private content stay outside telemetry.
func TestMCPResultDeliveryAuditedOnce(t *testing.T) {
	exporter := installStreamableTestTracer(t)
	sess := &mcpSession{transport: "sse", pendingRequests: map[string]struct{}{"1": {}}}
	response := `{"result":{"_meta":{"com.usefused/execute":{"delivery":"stored","retained_reads":2,"unavailable_reads":1,"output_budget_bytes":1024,"secret":"private"}},"content":[{"type":"text","text":"private message fused-result:private /transactions merchant"}]}}`
	completeMCPToolCall(sess, "1", response, "")
	completeMCPToolCall(sess, "1", response, "")
	spans := exporter.GetSpans()
	// Only the winner of pending-call completion may emit an event.
	if len(spans) != 1 || spans[0].Name != "engine.sandbox.mcp.result_delivery" {
		t.Fatalf("delivery span count = %d", len(spans))
	}
	want := map[string]string{
		"actor.type": "agent", "mcp.transport": "sse", "mcp.result.delivery": "stored",
		"mcp.result.retained_reads": "2", "mcp.result.unavailable_reads": "1",
		"mcp.result.output_budget_bytes": "1024",
	}
	// Exact allowlisting catches private values and unintended new dimensions together.
	if !reflect.DeepEqual(mcpSearchTestAttributes(spans[0]), want) {
		t.Fatal("delivery telemetry differs from its privacy allowlist")
	}
}

// TestMCPResultDeliveryRejectsUntrustedMetadata keeps scripts, unknown states, and invalid counters out of OTEL.
func TestMCPResultDeliveryRejectsUntrustedMetadata(t *testing.T) {
	responses := []string{
		`not json`, `{}`, `{"result":{"content":[{"_meta":{"com.usefused/execute":{"delivery":"stored"}}}]}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"private text"}}}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"inline","retained_reads":-1}}}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"error","retained_reads":100001}}}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"error","retained_reads":0,"unavailable_reads":1}}}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"inline","output_budget_bytes":1023}}}}`,
		`{"result":{"_meta":{"com.usefused/execute":{"delivery":"inline","output_budget_bytes":65537}}}}`,
	}
	for _, response := range responses {
		// Rejected metadata must not be interpreted as a successful default observation.
		if _, ok := mcpRuntimeResultDelivery(response); ok {
			t.Fatal("untrusted delivery metadata was admitted")
		}
	}
}

// TestMCPEffectiveOutputLimitUsesTrustedBudget keeps smaller client ceilings accurate without reading error payload fields.
func TestMCPEffectiveOutputLimitUsesTrustedBudget(t *testing.T) {
	response := `{"result":{"_meta":{"com.usefused/execute":{"delivery":"error","output_budget_bytes":1024}}}}`
	// Valid metadata tightens the audit ceiling but never expands the compiled error cap.
	if mcpEffectiveOutputLimit(response, 8<<10) != 1024 || mcpEffectiveOutputLimit(response, 512) != 512 {
		t.Fatal("effective output limit did not honor the tighter policy")
	}
	// An error payload's own budget is untrusted; absent host metadata preserves the compiled ceiling.
	if mcpEffectiveOutputLimit(`{"result":{"content":[{"text":"max_bytes:1"}]}}`, 8<<10) != 8<<10 {
		t.Fatal("content changed trusted output policy")
	}
}
