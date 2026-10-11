package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/features"
	"github.com/Usefused/engine/internal/shared/models"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestImportedMCPDisabledBoundaries proves the gate precedes storage, credential reads, and provider dispatch.
func TestImportedMCPDisabledBoundaries(t *testing.T) {
	t.Setenv(features.ImportedMCPEnvironment, "false")
	for _, handler := range []http.HandlerFunc{GetMCPCatalogHandler(nil), DiscoverMCPCatalogHandler(nil, nil, nil), ApplyMCPCatalogHandler(nil, nil)} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodPost, "/", nil))
		// Nil dependencies would panic if a disabled request crossed the gate.
		if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), features.ErrImportedMCPDisabled.Error()) {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	engine := &EngineGRPCServer{}
	_, err := engine.InvokeImportedMCP(context.Background(), auth.RuntimeIdentity{}, "tools/call", "cached-tool", nil)
	// Even a capability cached by an existing session must fail before loading credentials.
	if !errors.Is(err, features.ErrImportedMCPDisabled) {
		t.Fatal(err)
	}
	scope, err := engine.ImportedMCPScope(context.Background(), auth.RuntimeIdentity{})
	// Stored imports become undiscoverable without deleting the app's persisted selection.
	if err != nil || len(scope) != 0 {
		t.Fatalf("scope=%v err=%v", scope, err)
	}
}

// TestImportedMCPAdmissionToggle covers new plans and plans prepared before the operator disabled imports.
func TestImportedMCPAdmissionToggle(t *testing.T) {
	doc := sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{"demo": {MCP: &models.ImportedMCPSelection{Tools: []string{"echo"}}}}}
	t.Setenv(features.ImportedMCPEnvironment, "true")
	// Explicit opt-in preserves existing imported selection admission.
	if err := validateImportedMCPFeature(doc); err != nil {
		t.Fatal(err)
	}
	t.Setenv(features.ImportedMCPEnvironment, "false")
	// Browser, GraphQL, and CLI planning share the same restriction.
	if err := validateMCPAppRestrictions(doc, "mcp"); !errors.Is(err, features.ErrImportedMCPDisabled) {
		t.Fatal(err)
	}
	desired, _ := json.Marshal(doc)
	_, _, err := decodeAppApplyPlan(context.Background(), nil, nil, &store.ConfigPlan{DesiredState: desired, ResolvedPayload: json.RawMessage(`{}`)}, "mcp")
	// Delayed apply must reject before any bucket reads or app mutation.
	if err == nil || !strings.Contains(err.Error(), features.ErrImportedMCPDisabled.Error()) {
		t.Fatal(err)
	}
	doc.Services["demo"] = sdkConfigServiceDoc{Operations: []string{"listCustomers"}}
	// Endpoint-based MCP apps remain usable while the preview is off.
	if err := validateMCPAppRestrictions(doc, "mcp"); err != nil {
		t.Fatal(err)
	}
}

// TestImportedMCPUIFlag reflects the effective Engine setting rather than caller-supplied UI configuration.
func TestImportedMCPUIFlag(t *testing.T) {
	for _, enabled := range []string{"false", "true"} {
		t.Setenv(features.ImportedMCPEnvironment, enabled)
		response := httptest.NewRecorder()
		serveEmbeddedUIRuntimeConfig(response, httptest.NewRequest(http.MethodGet, "/env.js", nil), EmbeddedUIRuntimeConfig{ImportedMCPEnabled: true})
		// An old or manually supplied shell setting cannot override the Engine's operator gate.
		if !strings.Contains(response.Body.String(), `"IMPORTED_MCP_ENABLED":`+enabled) {
			t.Fatal(response.Body.String())
		}
	}
}
