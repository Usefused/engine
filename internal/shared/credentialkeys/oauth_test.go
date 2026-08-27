package credentialkeys

import "testing"

// TestOAuthApplication verifies the sole naming contract used by persistence and runtime resolution.
func TestOAuthApplication(t *testing.T) {
	clientID, clientSecret, ok := OAuthApplication(" oauth2 ")
	// The normalized scheme must produce both exact family members in fixed order.
	if !ok || clientID != "oauth2_client_id" || clientSecret != "oauth2_client_secret" {
		t.Fatalf("OAuthApplication() = %q, %q, %v", clientID, clientSecret, ok)
	}
	_, _, ok = OAuthApplication(" ")
	// Empty names are not valid storage namespaces.
	if ok {
		t.Fatal("OAuthApplication accepted a blank auth name")
	}
}

// TestOAuthApplicationField rejects extensible key creation while preserving semantic CLI fields.
func TestOAuthApplicationField(t *testing.T) {
	key, ok := OAuthApplicationField("oidc", OAuthClientSecretField)
	// Only the reviewed client fields may map into deterministic rows.
	if !ok || key != "oidc_client_secret" {
		t.Fatalf("OAuthApplicationField() = %q, %v", key, ok)
	}
	_, ok = OAuthApplicationField("oidc", "redirect_uri")
	// Redirect URI is Engine configuration, never caller-owned credential input.
	if ok {
		t.Fatal("OAuthApplicationField accepted redirect_uri")
	}
}
