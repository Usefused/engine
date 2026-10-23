package api

import (
	"github.com/google/uuid"
	"testing"
)

// TestWorkflowAuthoredPinsRecoverNames verifies that display normalization never changes executable bindings.
func TestWorkflowAuthoredPinsRecoverNames(t *testing.T) {
	id, version := uuid.New(), uuid.New()
	pins := []workflowServicePin{{Key: "Mobile Preview Health", ServiceID: id, ServiceVersionID: version}}
	operations := []unifiedAppManifestOperation{{Service: "@preview/health", ServiceID: id, ServiceVersionID: version}, {Service: "unrelated", ServiceID: id, ServiceVersionID: uuid.New()}}
	got, err := matchWorkflowAuthoredPins(pins, operations)
	// A different version's alias must never influence this immutable source export.
	if err != nil || len(got) != 1 || got[0].AuthoredKey != "@preview/health" || got[0].Key != pins[0].Key || pins[0].AuthoredKey != "" {
		t.Fatalf("pins = %#v, err = %v", got, err)
	}
	operations = append(operations, unifiedAppManifestOperation{Service: "conflicting", ServiceID: id, ServiceVersionID: version})
	// Multiple authored aliases cannot be reconstructed from one normalized configuration entry.
	if _, err := matchWorkflowAuthoredPins(pins, operations); err == nil {
		t.Fatal("ambiguous aliases accepted")
	}
}
