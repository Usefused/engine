package api

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"testing"
)

// attachmentRuntimeFixture exposes only stored bindings so tests detect accidental target lookups on denial.
type attachmentRuntimeFixture struct {
	store.Store
	bindings []models.UnifiedAppBinding
	reads    int
}

// ResolveUnifiedAppBindings is unused by runtime delegation tests.
func (f *attachmentRuntimeFixture) ResolveUnifiedAppBindings(context.Context, uuid.UUID, map[string]models.UnifiedAppReference) ([]models.UnifiedAppBinding, error) {
	return f.bindings, nil
}

// ReadUnifiedAppBindings counts exact consumer reads without providing workspace-wide discovery.
func (f *attachmentRuntimeFixture) ReadUnifiedAppBindings(context.Context, uuid.UUID, uuid.UUID) ([]models.UnifiedAppBinding, error) {
	f.reads++
	return f.bindings, nil
}

// TestUnifiedAppAttachmentValidation accepts reference-only consumers while rejecting recursive or unversioned scope.
func TestUnifiedAppAttachmentValidation(t *testing.T) {
	doc := sdkConfigDocument{Kind: "sdk", UnifiedApps: map[string]models.UnifiedAppReference{"lookup": {Name: "Customer lookup", Version: "1.0.0"}}}
	if err := validateAttachedAppServices(doc); err != nil {
		t.Fatal(err)
	}
	doc.Kind = "unified_app"
	if validateAttachedAppServices(doc) == nil {
		t.Fatal("recursive app accepted")
	}
	doc.Kind = "mcp"
	doc.UnifiedApps["lookup"] = models.UnifiedAppReference{Name: "Customer lookup", Version: "latest"}
	if validateAttachedAppServices(doc) == nil {
		t.Fatal("latest accepted")
	}
}

// TestUnifiedAppAttachmentTokenBoundary rejects ungranted aliases before inspecting target runtime or code.
func TestUnifiedAppAttachmentTokenBoundary(t *testing.T) {
	fixture := &attachmentRuntimeFixture{bindings: []models.UnifiedAppBinding{{Alias: "lookup"}}}
	server := &EngineGRPCServer{store: fixture}
	identity := auth.RuntimeIdentity{Kind: store.AppKindSDK, TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"getCustomer"}}}
	if _, err := server.ExecuteAttachedUnifiedApp(context.Background(), identity, "lookup", json.RawMessage(`{}`)); err == nil {
		t.Fatal("token widened")
	}
	if fixture.reads != 0 {
		t.Fatal("denied token reached attachment storage")
	}
	identity.TokenPolicy = store.AppTokenPolicy{AllowAll: true}
	if _, err := server.ExecuteAttachedUnifiedApp(context.Background(), identity, "unknown", json.RawMessage(`{}`)); err == nil {
		t.Fatal("unknown alias accepted")
	}
	if fixture.reads != 1 {
		t.Fatal("unknown alias bypassed exact consumer membership")
	}
}
