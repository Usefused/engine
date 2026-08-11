package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestStaticSecretKeysForMTLS documents the bucket secret contract for
// transport credentials: one selected auth prefix, two exact service-scoped keys.
func TestStaticSecretKeysForMTLS(t *testing.T) {
	got := staticSecretKeysForAuth(fusedobject.AuthConfig{Name: "clientCert", Type: "mutualTLS"})

	want := []string{"clientCert_cert", "clientCert_key"}
	if len(got) != len(want) {
		t.Fatalf("got keys %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got keys %#v, want %#v", got, want)
		}
	}
}

func TestCredentialsWithSelectionAuthAddsOnlyResolvedMetadata(t *testing.T) {
	input := map[string]any{"fused_end_user_ref": "customer-42"}
	secured := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "oauthAuth"}}}}
	resolved := credentialsWithSelectionAuth(input, models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth"}, secured)
	if resolved["fused_auth_type"] != "oauth" || resolved["fused_auth_name"] != "oauthAuth" {
		t.Fatalf("selection auth was not injected: %#v", resolved)
	}
	if _, mutated := input["fused_auth_type"]; mutated {
		t.Fatal("selection auth must not mutate a shared caller envelope")
	}
}

func TestCredentialsWithSelectionAuthPreservesAnonymousOperation(t *testing.T) {
	input := map[string]any{"fused_end_user_ref": "customer-42"}
	requirements := authrouting.Requirements{
		{Schemes: []authrouting.Requirement{}},
		{Schemes: []authrouting.Requirement{{Scheme: "oauthAuth"}}},
	}
	resolved := credentialsWithSelectionAuth(input, models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth"}, requirements)
	if _, exists := resolved["fused_auth_type"]; exists {
		t.Fatalf("plan-inferred auth selector was injected into an anonymous operation: %#v", resolved)
	}
}

func TestCredentialsWithSelectionAuthPreservesExplicitPerCallSelector(t *testing.T) {
	input := map[string]any{"fused_auth_type": "basic", "fused_auth_name": "basicAuth"}
	secured := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "oauthAuth"}}}}
	resolved := credentialsWithSelectionAuth(input, models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth"}, secured)
	if resolved["fused_auth_type"] != "basic" || resolved["fused_auth_name"] != "basicAuth" {
		t.Fatalf("explicit per-call auth selector was overwritten: %#v", resolved)
	}
}

func TestCredentialsWithSelectionAuthLeavesMixedPolicyUnpinned(t *testing.T) {
	input := map[string]any{"fused_end_user_ref": "customer-42"}
	secured := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "apiKey"}}}}
	selection := models.SDKSelection{RequiredAuth: []models.SDKRequiredAuth{
		{AuthType: "api_key", AuthName: "apiKey"}, {AuthType: "oauth", AuthName: "oauthAuth"},
	}}
	resolved := credentialsWithSelectionAuth(input, selection, secured)
	if _, exists := resolved["fused_auth_name"]; exists {
		t.Fatalf("mixed operation policy invented a runtime selector: %#v", resolved)
	}
}

func TestOrderedStaticSecretAlternativesKeepsMTLSWithConnectedOAuth(t *testing.T) {
	auths := fusedobject.AuthConfigs{
		{Name: "oauthAuth", Type: "oauth2"},
		{Name: "clientCertificate", Type: "mutualTLS"},
	}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{
		{Scheme: "oauthAuth"}, {Scheme: "clientCertificate"},
	}}}
	credentials := map[string]any{
		"fused_auth_type": "oauth", "fused_auth_name": "oauthAuth", "fused_end_user_ref": "customer-42",
	}
	alternatives, err := orderedStaticSecretAlternatives(auths, requirements, credentials)
	if err != nil {
		t.Fatal(err)
	}
	if len(alternatives) != 1 || len(alternatives[0].Required) != 2 || alternatives[0].Required[0] != "clientCertificate_cert" || alternatives[0].Required[1] != "clientCertificate_key" {
		t.Fatalf("OAuth+mTLS static material = %#v", alternatives)
	}
}

func TestOrderedStaticSecretAlternativesKeepsEveryAPIKeyInANDSet(t *testing.T) {
	auths := fusedobject.AuthConfigs{{Name: "apiKey", Type: "apiKey"}, {Name: "apiToken", Type: "apiKey"}}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "apiKey"}, {Scheme: "apiToken"}}}}
	alternatives, err := orderedStaticSecretAlternatives(auths, requirements, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(alternatives) != 1 || len(alternatives[0].Required) != 2 || alternatives[0].Required[0] != "apiKey" || alternatives[0].Required[1] != "apiToken" {
		t.Fatalf("API key AND token material = %#v", alternatives)
	}
}

func TestConnectedAuthResolutionRequiredKeepsAnonymousCallsAnonymous(t *testing.T) {
	anonymous := authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
	if connectedAuthResolutionRequired("customer-42", map[string]any{}, anonymous) {
		t.Fatal("end-user context alone must not authenticate an anonymous operation")
	}
	if !connectedAuthResolutionRequired("customer-42", map[string]any{"fused_auth_type": "oauth"}, anonymous) {
		t.Fatal("an explicit per-call OAuth selector must remain authoritative")
	}
	if connectedAuthResolutionRequired("customer-42", map[string]any{"fused_auth_type": "api_key"}, anonymous) {
		t.Fatal("a static selector must not trigger connected-auth lookup")
	}
}
