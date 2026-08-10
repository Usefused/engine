package sandbox

import (
	"testing"

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
	resolved := credentialsWithSelectionAuth(input, models.SDKSelection{AuthType: "oauth", AuthName: "oauthAuth"})
	if resolved["fused_auth_type"] != "oauth" || resolved["fused_auth_name"] != "oauthAuth" {
		t.Fatalf("selection auth was not injected: %#v", resolved)
	}
	if _, mutated := input["fused_auth_type"]; mutated {
		t.Fatal("selection auth must not mutate a shared caller envelope")
	}
}
