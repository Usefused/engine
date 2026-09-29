package sandbox

import (
	"context"
	"strings"
	"testing"
)

// TestInspectCapabilityBundleReadsActualDeclarations checks child-side evaluation independent of host OS isolation availability.
func TestInspectCapabilityBundleReadsActualDeclarations(t *testing.T) {
	const source = `globalThis.FusedExecutionManifest={schemaVersion:1,inputSchema:{type:"object"},outputSchema:{type:"object"},searchable:[],selectedOperations:[]};globalThis.FusedUnifiedApp={input:{parse(v){return v}},output:{parse(v){return v}},execute:async()=>({})};`
	manifest, err := inspectCapabilityBundleInProcess(context.Background(), []byte(source))
	// The inspected value must come from executing the immutable script itself.
	if err != nil || string(manifest) != `{"schemaVersion":1,"inputSchema":{"type":"object"},"outputSchema":{"type":"object"},"searchable":[],"selectedOperations":[]}` {
		t.Fatalf("manifest = %s, %v", manifest, err)
	}
}

// TestInspectCapabilityBundleHasNoHostBridge ensures child declarations cannot call provider or DB effects.
func TestInspectCapabilityBundleHasNoHostBridge(t *testing.T) {
	const source = `globalThis.__fusedHost.fetch("{}"); globalThis.FusedExecutionManifest={schemaVersion:1};`
	_, err := inspectCapabilityBundleInProcess(context.Background(), []byte(source))
	// A top-level effect must fail without exposing a host primitive to the build evaluator.
	if err == nil || !strings.Contains(err.Error(), "evaluation failed") {
		t.Fatalf("top-level effect error = %v", err)
	}
}
