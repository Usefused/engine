package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestSelectedAuthConfigsForExecutionSelectsBasic keeps applyAuth's first-auth
// contract honest when a call explicitly chooses a static auth family.
func TestSelectedAuthConfigsForExecutionSelectsBasic(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "api", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "basicAuth", Type: "http", Scheme: "basic"},
	}

	selected, selectedType, err := selectedAuthConfigsForExecution(auths, map[string]any{
		"fused_auth_type": "basic",
	})

	if err != nil {
		t.Fatalf("selectedAuthConfigsForExecution failed: %v", err)
	}
	if selectedType != "basic" {
		t.Fatalf("expected selected type basic, got %q", selectedType)
	}
	if len(selected) != 1 || selected[0].Name != "basicAuth" {
		t.Fatalf("expected only basic auth selected, got %#v", selected)
	}
}

// TestSelectedAuthConfigsForExecutionKeepsDefault proves omitted auth_type
// preserves legacy behavior where the first configured auth is applied.
func TestSelectedAuthConfigsForExecutionKeepsDefault(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "api", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "basicAuth", Type: "http", Scheme: "basic"},
	}

	selected, selectedType, err := selectedAuthConfigsForExecution(auths, nil)

	if err != nil {
		t.Fatalf("selectedAuthConfigsForExecution failed: %v", err)
	}
	if selectedType != "api_key" {
		t.Fatalf("expected default selected type api_key, got %q", selectedType)
	}
	if len(selected) != 2 {
		t.Fatalf("expected default auth list to remain intact, got %#v", selected)
	}
}

// TestSelectedAuthConfigsForExecutionRejectsUnknown turns misconfigured app
// calls into a local Engine error before dispatching with the wrong auth.
func TestSelectedAuthConfigsForExecutionRejectsUnknown(t *testing.T) {
	_, _, err := selectedAuthConfigsForExecution(models.AuthConfigs{
		{Name: "api", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
	}, map[string]any{
		"fused_auth_type": "basic",
	})

	if err == nil {
		t.Fatalf("expected unknown auth type error")
	}
}

// TestSelectedAuthConfigsForExecutionRejectsOAuth2Selector keeps the SDK
// surface on the public oauth family instead of leaking OpenAPI's oauth2 name.
func TestSelectedAuthConfigsForExecutionRejectsOAuth2Selector(t *testing.T) {
	_, _, err := selectedAuthConfigsForExecution(models.AuthConfigs{
		{Name: "oauthAuth", Type: "oauth2"},
	}, map[string]any{
		"fused_auth_type": "oauth2",
	})

	if err == nil {
		t.Fatalf("expected oauth2 selector to be rejected")
	}
}

// TestSelectedAuthConfigsForExecutionRejectsImportedSelectorAliases prevents
// user-facing SDK selectors from accepting registry/OpenAPI spellings.
func TestSelectedAuthConfigsForExecutionRejectsImportedSelectorAliases(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "api", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "oidc", Type: "openIdConnect"},
	}
	for _, selector := range []string{"apikey", "openidconnect", "open_id_connect"} {
		if _, _, err := selectedAuthConfigsForExecution(auths, map[string]any{
			"fused_auth_type": selector,
		}); err == nil {
			t.Fatalf("expected selector %q to be rejected", selector)
		}
	}
}

// TestSelectedAuthConfigsForExecutionSelectsMTLS proves public mtls selection
// can choose imported mutualTLS metadata without exposing that spelling to SDKs.
func TestSelectedAuthConfigsForExecutionSelectsMTLS(t *testing.T) {
	selected, selectedType, err := selectedAuthConfigsForExecution(models.AuthConfigs{
		{Name: "api", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "clientCert", Type: "mutualTLS"},
	}, map[string]any{
		"fused_auth_type": "mtls",
	})

	if err != nil {
		t.Fatalf("selectedAuthConfigsForExecution failed: %v", err)
	}
	if selectedType != "mtls" {
		t.Fatalf("expected selected type mtls, got %q", selectedType)
	}
	if len(selected) != 1 || selected[0].Name != "clientCert" {
		t.Fatalf("expected only mTLS auth selected, got %#v", selected)
	}
}

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
	resolved := credentialsWithSelectionAuth(input, models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth"})
	if resolved["fused_auth_type"] != "oauth" || resolved["fused_auth_name"] != "oauthAuth" {
		t.Fatalf("selection auth was not injected: %#v", resolved)
	}
	if _, mutated := input["fused_auth_type"]; mutated {
		t.Fatal("selection auth must not mutate a shared caller envelope")
	}
}
