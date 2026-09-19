package oauthprovider

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestAppConsentRequiresExplicitType prevents an Owner's broad grants from widening a client's request.
func TestAppConsentRequiresExplicitType(t *testing.T) {
	workspace := accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: uuid.New()}
	grants := []accesscontrol.Grant{}
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: workspace})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	// A fully privileged actor makes the client/request ceiling the tested boundary.
	if err != nil {
		t.Fatal(err)
	}
	actor := accesscontrol.Actor{Authorization: snapshot}
	client := store.OAuthClient{AllowedScopes: []string{"app.mcp.create", "app.sdk.create", "app.api.create", "app.webhook.create"}}
	service := &Service{}
	granted, err := service.resolveGrantableScope(t.Context(), actor, client, []string{"app.mcp.create"})
	// Only the requested MCP grant belongs in the resulting consent.
	if err != nil || len(granted) != 1 || granted[0] != "app.mcp.create" {
		t.Fatalf("scope=%v error=%v", granted, err)
	}
	for _, requested := range [][]string{nil, {"app.create"}, {"app.manage"}} {
		_, err := service.resolveGrantableScope(t.Context(), actor, client, requested)
		// Missing and retired broad scopes must request fresh explicit consent.
		if !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("scope %v: %v", requested, err)
		}
	}
}
