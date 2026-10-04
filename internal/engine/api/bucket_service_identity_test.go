package api

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/google/uuid"
)

// TestBucketServiceIdentities verifies one authorized lookup labels orphaned credential families without exposing unrelated services.
func TestBucketServiceIdentities(t *testing.T) {
	readable, denied := uuid.New(), uuid.New()
	actor := actorWithResourcePermissions(t, uuid.New(), accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: readable}})
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	verifier := &mockVerifier{visibilityOverrides: map[uuid.UUID]sandbox.ServiceVisibility{readable: {Name: "Gmail", Slug: "gmail", IsOwner: true}}}
	items := []map[string]interface{}{
		{"service_id": readable.String(), "key_name": "oauth2"},
		{"service_id": readable.String(), "key_name": "admin_oauth"},
		{"service_id": denied.String()},
		{"service_id": uuid.Nil.String()},
	}
	got := enrichBucketServiceIdentities(ctx, verifier, items)
	// Duplicate credential families must share one batch entry, with denied and generic IDs excluded.
	if !reflect.DeepEqual(verifier.visibilityCalls, [][]uuid.UUID{{readable}}) {
		t.Fatalf("unexpected lookup scope: %v", verifier.visibilityCalls)
	}
	// Metadata on both families remains distinct while the same service identity is attached.
	if got[0]["service_name"] != "Gmail" || got[1]["service_slug"] != "gmail" || got[1]["key_name"] != "admin_oauth" || got[2]["service_name"] != nil || got[3]["service_name"] != nil {
		t.Fatalf("unexpected metadata: %v", got)
	}
}

// TestBucketServiceIdentityUnavailable confirms name lookup cannot make credential metadata unavailable or bypass service permissions.
func TestBucketServiceIdentityUnavailable(t *testing.T) {
	id := uuid.New()
	items := []map[string]interface{}{{"service_id": id.String(), "service_name": "Cached Gmail"}}
	verifier := &mockVerifier{visibilityErr: errors.New("offline")}
	actor := actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionServiceRead)
	ctx := accesscontrol.ContextWithActor(context.Background(), actor)
	got := enrichBucketServiceIdentities(ctx, verifier, items)
	// Offline Registry enrichment must retain the local label and the original page.
	if got[0]["service_name"] != "Cached Gmail" {
		t.Fatalf("lost local identity: %v", got)
	}
	verifier.visibilityCalls = nil
	actor = actorWithWorkspacePermissions(t, uuid.New(), accesscontrol.PermissionCredentialsMetadataRead)
	enrichBucketServiceIdentities(accesscontrol.ContextWithActor(context.Background(), actor), verifier, items)
	// Metadata-only access grants no service lookup, even when the secret contains its ID.
	if len(verifier.visibilityCalls) != 0 {
		t.Fatalf("unexpected unauthorized lookup: %v", verifier.visibilityCalls)
	}
}
