package sandbox

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/features"
	"github.com/Usefused/engine/internal/shared/models"
	"testing"
)

// TestDisabledImportedDiscovery withholds provider namespaces while preserving physical tools and saved definitions.
func TestDisabledImportedDiscovery(t *testing.T) {
	t.Setenv(features.ImportedMCPEnvironment, "false")
	scope := []models.SDKSelection{{ImportedMCP: &models.ImportedMCPBinding{
		Tools:             []json.RawMessage{json.RawMessage(`{"name":"echo"}`)},
		Prompts:           []json.RawMessage{json.RawMessage(`{"name":"summary"}`)},
		Resources:         []json.RawMessage{json.RawMessage(`{"uri":"docs://guide"}`)},
		ResourceTemplates: []json.RawMessage{json.RawMessage(`{"uriTemplate":"docs://{id}"}`)},
	}}}
	operations, err := appendImportedFixtureOperations([]FixtureOperation{{OperationID: "physical"}}, scope, store.AppTokenPolicy{AllowAll: true})
	// Disabling imports must never suppress ordinary Fused tools.
	if err != nil || len(operations) != 1 || operations[0].OperationID != "physical" {
		t.Fatalf("operations=%v err=%v", operations, err)
	}
	caps := importedProtocolCapabilities(scope, auth.RuntimeIdentity{TokenPolicy: store.AppTokenPolicy{AllowAll: true}})
	// Both protocol generations derive native discovery from this projection.
	if len(caps) != 0 {
		t.Fatalf("caps=%v", caps)
	}
	// The gate is reversible because stored metadata is not rewritten.
	if len(scope[0].ImportedMCP.Tools) != 1 {
		t.Fatal("saved selection changed")
	}
}
