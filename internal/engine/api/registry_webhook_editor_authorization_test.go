package api

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// TestRegistryWebhookEditorRequiresImportPermission keeps read-only catalogue actors out of the authoring path.
func TestRegistryWebhookEditorRequiresImportPermission(t *testing.T) {
	for _, permission := range []accesscontrol.Permission{accesscontrol.PermissionCatalogueRead, accesscontrol.PermissionCatalogueImport} {
		actor := registryPolicyActor(t, permission)
		ctx := accesscontrol.ContextWithActor(context.Background(), actor)
		_, err := authorizeRegistryGraphQLOperation(ctx, []byte(`{"query":"query { serviceWebhookEditor(service_id:\"x\",version:\"v1\") { revision } }"}`))
		// Draft loading uses exactly the same grant as import plan/apply, not a stronger unrelated grant.
		if permission == accesscontrol.PermissionCatalogueImport {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		// A viewer cannot make the owner-scoped Registry projection run through a forged browser request.
		if !errors.Is(err, accesscontrol.ErrPermissionDenied) {
			t.Fatalf("read-only editor=%v", err)
		}
	}
}
