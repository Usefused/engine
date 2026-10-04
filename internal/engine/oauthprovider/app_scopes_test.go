package oauthprovider

import (
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestAppConsentRequiresExplicitType prevents an Owner's broad grants from widening a client's request.
func TestAppConsentRequiresExplicitType(t *testing.T) {
	workspace := accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: uuid.New()}
	grants := []accesscontrol.Grant{}
	// A workspace owner provides the broadest possible permissions for this ceiling test.
	for _, permission := range accesscontrol.AllPermissions() {
		grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: workspace})
	}
	snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, grants...)
	// A fully privileged actor makes the client/request ceiling the tested boundary.
	if err != nil {
		t.Fatal(err)
	}
	actor := browserActor(t)
	actor.Authorization = snapshot
	client := confidentialClient()
	client.AllowedScopes = []string{"app.mcp.create", "app.sdk.create", "app.api.create", "app.webhook.create"}
	service, _ := newTestService(t, &fakeOAuthStore{client: client})
	req := authorizeRequest(client)
	req.Scope = []string{"app.mcp.create"}
	result, err := service.Authorize(t.Context(), actor, req)
	granted := result.Scope
	// Only the requested MCP grant belongs in the resulting consent.
	if err != nil || len(granted) != 1 || granted[0] != "app.mcp.create" {
		t.Fatalf("scope=%v error=%v", granted, err)
	}
	// Omitted or retired broad scopes cannot expand explicit app-type consent.
	for _, requested := range [][]string{nil, {"app.create"}, {"app.manage"}} {
		req.Scope = requested
		_, err := service.Authorize(t.Context(), actor, req)
		// Missing and retired broad scopes must request fresh explicit consent.
		if !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("scope %v: %v", requested, err)
		}
	}
}
