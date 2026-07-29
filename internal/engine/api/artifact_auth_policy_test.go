package api

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestResolveSelectionAuthPolicyPinsProviderScheme(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", ConnectScopes: []string{"read"}}
	auths := fusedobject.AuthConfigs{
		{Name: "basicAuth", Type: "http", Scheme: "basic"},
		{Name: "oauthAuth", Type: "oauth2", Scopes: []string{"read", "write"}},
	}

	if err := resolveSelectionAuthPolicy(&selection, auths); err != nil {
		t.Fatalf("resolveSelectionAuthPolicy() error = %v", err)
	}
	if selection.AuthType != "oauth" || selection.AuthName != "oauthAuth" {
		t.Fatalf("unexpected resolved auth policy: %#v", selection)
	}
}

func TestResolveSelectionAuthPolicyRejectsBroaderScope(t *testing.T) {
	selection := models.SDKSelection{ServiceID: uuid.New(), AuthType: "oauth", ConnectScopes: []string{"admin"}}
	err := resolveSelectionAuthPolicy(&selection, fusedobject.AuthConfigs{{Name: "oauthAuth", Type: "oauth2", Scopes: []string{"read"}}})
	if err == nil || !strings.Contains(err.Error(), "not provider-approved") {
		t.Fatalf("expected provider scope ceiling error, got %v", err)
	}
}
