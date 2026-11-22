package api

import (
	"encoding/json"
	"testing"
)

// TestReceiptStepsSkipsSnapshotsAndLabelsExternalCalls verifies snapshot checkpoints stay out of
// forward receipt steps while provider, Bridge, and nested Unified App calls retain readable metadata.
func TestReceiptStepsSkipsSnapshotsAndLabelsExternalCalls(t *testing.T) {
	host := &recordingCapabilityHost{
		calls: []capabilityReplayCall{
			{Ordinal: 1, Request: json.RawMessage(`{"snapshot":"load"}`), Completion: 1},
			{Ordinal: 2, Request: json.RawMessage(`{"service":"@acme/stripe","operation":"createCustomer","input":{}}`), Completion: 2},
			{Ordinal: 3, Request: json.RawMessage(`{"unifiedApp":"billing_customer","input":{}}`), Completion: 3},
			{Ordinal: 4, Request: json.RawMessage(`{"bridge":"local_printer","input":{}}`), Error: "failed", Completion: 4},
			{Ordinal: 5, Request: json.RawMessage(`{"snapshot":"save","state":{"ok":true}}`), Completion: 5},
		},
	}

	steps := host.receiptSteps()
	// Snapshot load/save entries must be excluded so only external forward calls appear in receipts.
	if len(steps) != 3 {
		t.Fatalf("len(steps) = %d, want 3: %#v", len(steps), steps)
	}
	// Provider operations keep their call_N join target and surface the selected operation name.
	if steps[0].Target != "call_2" || steps[0].Operation != "createCustomer" || steps[0].Kind != "operation" || steps[0].Status != "success" {
		t.Fatalf("provider step = %#v, want call_2/createCustomer/operation/success", steps[0])
	}
	// Nested Unified App calls retain their alias and unified_app kind so the UI explains their separate receipt stream.
	if steps[1].Target != "call_3" || steps[1].Operation != "billing_customer" || steps[1].Kind != "unified_app" || steps[1].Status != "success" {
		t.Fatalf("nested app step = %#v, want call_3/billing_customer/unified_app/success", steps[1])
	}
	// Failed Bridge calls retain their bridge:<alias> label and error classification even without a physical receipt.
	if steps[2].Target != "call_4" || steps[2].Operation != "bridge:local_printer" || steps[2].Kind != "bridge" || steps[2].Status != "error" || steps[2].ErrorCode != "execution_failed" {
		t.Fatalf("bridge step = %#v, want call_4/bridge:local_printer/bridge/error", steps[2])
	}
}

// TestReceiptStepDescriptorDropsInvalidOperationNames ensures malformed authored names never fail receipt validation.
func TestReceiptStepDescriptorDropsInvalidOperationNames(t *testing.T) {
	operation, kind, include := receiptStepDescriptor(json.RawMessage(`{"operation":"bad\nname"}`))
	// Invalid operation names are omitted while preserving the step itself for auditability.
	if !include || kind != "operation" || operation != "" {
		t.Fatalf("got (%q, %q, %v), want (\"\", \"operation\", true)", operation, kind, include)
	}
}
