package connectauth

import (
	"strings"
	"testing"
)

// TestAuthorizationPrincipalSelection prevents a missing user token from silently selecting the bot's authority.
func TestAuthorizationPrincipalSelection(t *testing.T) {
	body := []byte(`{"access_token":"bot-test","scope":"bot-scope","team":{"id":"T-test"},"authed_user":{"access_token":"user-test","scope":"app_configurations:write","refresh_token":"refresh-test"}}`)
	token, err := decodeSelectedTokenResponse("application/json", body, []string{"authed_user"})
	// All normalized credential fields must belong to the same selected principal while ownership proof retains outer claims.
	if err != nil || token.AccessToken != "user-test" || token.RefreshToken != "refresh-test" || token.Scope != "app_configurations:write" || !strings.Contains(string(token.RawResponse), `"team"`) {
		t.Fatalf("principal mapping failed: %v", err)
	}
	for _, bad := range []string{`{"access_token":"bot-test"}`, `{"access_token":"bot-test","authed_user":null}`, `{"authed_user":[]}`, `{"authed_user":{"scope":"app_configurations:write"}}`} {
		_, err := decodeSelectedTokenResponse("application/json", []byte(bad), []string{"authed_user"})
		// A declared but unusable principal must fail instead of falling back or persisting an incomplete connection.
		if err == nil {
			t.Fatal("invalid selected principal was accepted")
		}
	}
	standard, err := decodeSelectedTokenResponse("application/json", body, nil)
	// Unmapped bot contracts and standard refresh responses retain the established top-level behavior.
	if err != nil || standard.AccessToken != "bot-test" {
		t.Fatalf("standard token mapping changed: %v", err)
	}
}
