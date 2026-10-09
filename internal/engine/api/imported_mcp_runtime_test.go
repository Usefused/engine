package api

import (
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"testing"
)

// importedSelectionFixture supplies all protocol namespaces without provider credentials.
func importedSelectionFixture() models.SDKSelection {
	return models.SDKSelection{ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SchemaVersion: models.AppSelectionSchemaVersion, ImportedMCP: &models.ImportedMCPBinding{RevisionID: uuid.New(), URL: "https://example.com/mcp", Tools: []json.RawMessage{json.RawMessage(`{"name":"echo","inputSchema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}}`)}, Prompts: []json.RawMessage{json.RawMessage(`{"name":"summary","arguments":[{"name":"topic","required":true}]}`)}, Resources: []json.RawMessage{json.RawMessage(`{"name":"Guide","uri":"fixture://guide"}`)}, ResourceTemplates: []json.RawMessage{json.RawMessage(`{"name":"Issue","uriTemplate":"fixture://issues/{id}"}`)}}}
}

// TestImportedRuntimeMembership proves token, service, type and template boundaries precede provider dispatch.
func TestImportedRuntimeMembership(t *testing.T) {
	selection := importedSelectionFixture()
	scope := []models.SDKSelection{selection}
	identity := auth.RuntimeIdentity{TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	tool := models.ImportedMCPOperation(selection.ServiceID, "tool", "echo")
	_, raw, err := findImportedMCP(scope, identity, "tools/call", tool, json.RawMessage(`{"text":"hello"}`))
	// Successful resolution replaces only the routing name and preserves tool arguments.
	if err != nil || string(raw) != `{"arguments":{"text":"hello"},"name":"echo"}` {
		t.Fatalf("%s %v", raw, err)
	}
	for _, tc := range []struct {
		method, name, args string
		allowed            bool
	}{
		{"tools/call", tool, `{"text":7}`, false},
		{"tools/call", models.ImportedMCPOperation(uuid.New(), "tool", "echo"), `{}`, false},
		{"tools/call", models.ImportedMCPOperation(selection.ServiceID, "tool", "unknown"), `{}`, false},
		{"prompts/get", models.ImportedMCPOperation(selection.ServiceID, "prompt", "summary"), `{"topic":"hello"}`, true},
		{"resources/read", sandbox.ImportedMCPResourcePrefix(selection.ServiceID) + "fixture://guide", `{}`, true},
		{"resources/read", sandbox.ImportedMCPResourcePrefix(selection.ServiceID) + "fixture://issues/42", `{}`, true},
		{"resources/read", sandbox.ImportedMCPResourcePrefix(selection.ServiceID) + "https://evil.example/private", `{}`, false},
	} {
		_, _, err := findImportedMCP(scope, identity, tc.method, tc.name, json.RawMessage(tc.args))
		// Only the reviewed namespace and matching input shape should resolve.
		if (err == nil) != tc.allowed {
			t.Fatalf("%s %s: %v", tc.method, tc.name, err)
		}
	}
	identity.TokenPolicy = store.AppTokenPolicy{AllowedOperations: []string{"other"}}
	// App selection alone never bypasses a restricted family token.
	if _, _, err := findImportedMCP(scope, identity, "tools/call", tool, json.RawMessage(`{"text":"hello"}`)); err == nil {
		t.Fatal("restricted token admitted")
	}
}

// TestImportedPlanSelection requires exact identities and copies only chosen metadata into immutable scope.
func TestImportedPlanSelection(t *testing.T) {
	selection := importedSelectionFixture()
	b := selection.ImportedMCP
	snapshot := &store.MCPCatalogSnapshot{ID: b.RevisionID, URL: b.URL, Catalog: mcpcatalog.Catalog{Tools: b.Tools, Prompts: b.Prompts, Resources: b.Resources, ResourceTemplates: b.ResourceTemplates}}
	request := &models.ImportedMCPSelection{RevisionID: b.RevisionID, Tools: []string{"echo"}, Resources: []string{"fixture://guide"}}
	pinned, err := selectImportedMCPBinding(snapshot, request)
	// Unselected prompts and templates cannot leak into the published app.
	if err != nil || len(pinned.Tools) != 1 || len(pinned.Prompts) != 0 || len(pinned.ResourceTemplates) != 0 {
		t.Fatalf("%+v %v", pinned, err)
	}
	snapshot.URL = "https://changed.example/mcp"
	snapshot.Catalog.Tools = nil
	// Later service refreshes cannot alter a deployment's reviewed connection or definitions.
	if pinned.URL != b.URL || len(pinned.Tools) != 1 {
		t.Fatal("pinned catalog mutated")
	}
	for _, names := range [][]string{{"unknown"}, {"echo", "echo"}} {
		// Unknown and duplicate grants fail without silently broadening selection.
		if _, err := selectImportedMCPItems(b.Tools, names, "name"); err == nil {
			t.Fatal("invalid selection admitted")
		}
	}
}

// TestImportedOnlyMCPConfig admits native-only services without requiring unrelated physical endpoints.
func TestImportedOnlyMCPConfig(t *testing.T) {
	doc := sdkConfigDocument{APIVersion: "fused/v1", Kind: "mcp", Name: "imported", Version: "1.0.0", Description: "Browse documentation", Bucket: "default", Services: map[string]sdkConfigServiceDoc{"demo": {Version: "1.0.0", MCP: &models.ImportedMCPSelection{RevisionID: uuid.New(), Tools: []string{"echo"}}}}}
	// Empty physical lists are intentional when imported capabilities supply the app's surface.
	if err := validateAppConfigDocument(doc, "mcp"); err != nil {
		t.Fatal(err)
	}
	doc.Kind = "sdk"
	doc.Description = ""
	doc.Language = "typescript"
	// Unsupported adapters must never silently drop the imported selection.
	if err := validateAppConfigDocument(doc, "sdk"); err == nil {
		t.Fatal("SDK silently accepted imported MCP")
	}
}
