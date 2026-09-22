package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// TestPromptCompositionRequiresCatalogueRead keeps the new Registry drafting route inside the existing discovery permission boundary.
func TestPromptCompositionRequiresCatalogueRead(t *testing.T) {
	body := []byte(`{"query":"query { draftPromptUnifiedOperation(q: \"goal\", selections: \"[]\") }"}`)
	ctx := accesscontrol.ContextWithActor(context.Background(), registryPolicyActor(t, accesscontrol.PermissionCatalogueRead))
	operation, err := authorizeRegistryGraphQLOperation(ctx, body)
	// Read-authorized callers may draft; app publication still uses its separate lifecycle permissions.
	if err != nil || operation != "query" {
		t.Fatalf("draft authorization: %q %v", operation, err)
	}
	ctx = accesscontrol.ContextWithActor(context.Background(), registryPolicyActor(t))
	_, err = authorizeRegistryGraphQLOperation(ctx, body)
	if !errors.Is(err, accesscontrol.ErrPermissionDenied) {
		// Missing discovery authority must fail before forwarding to Registry.
		t.Fatalf("expected permission denial, got %v", err)
	}
}
