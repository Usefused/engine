package api

import "testing"

// TestAPIRuntimeBucketAdmission permits only explicit package-free API definitions with bounded bucket scope.
func TestAPIRuntimeBucketAdmission(t *testing.T) {
	generate := false
	doc := sdkConfigDocument{Kind: "sdk", Generate: &generate, RuntimeBuckets: []string{"primary", "secondary"}}
	// API transport uses the same approved scope without inventing another app kind.
	if err := validateRuntimeBuckets(doc); err != nil {
		t.Fatal(err)
	}
	doc.MCP = &sdkMCPDeliveryDocument{}
	// MCP delivery has different connection binding rules and cannot inherit arbitrary bucket selection.
	if err := validateRuntimeBuckets(doc); err == nil {
		t.Fatal("MCP delivery accepted API bucket selection")
	}
	doc.MCP = nil
	generate = true
	// Existing generated SDK configurations retain their static bucket contract.
	if err := validateRuntimeBuckets(doc); err == nil {
		t.Fatal("generated SDK accepted runtime bucket scope")
	}
}
