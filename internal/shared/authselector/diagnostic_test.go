package authselector

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestNotFoundErrorExplainsExactChoices verifies every runtime transport can serialize the same correction contract.
func TestNotFoundErrorExplainsExactChoices(t *testing.T) {
	err := NewNotFoundError(
		Selection{AuthType: "basic", AuthName: "oauth2"},
		[]Selection{{AuthType: "oauth", AuthName: "googleOAuth"}, {AuthType: "api_key", AuthName: "providerKey"}},
	)
	encoded, marshalErr := json.Marshal(err)
	// The shared contract must remain directly serializable for generated SDK error frames.
	if marshalErr != nil {
		t.Fatalf("marshal selector error: %v", marshalErr)
	}
	message := string(encoded)
	// Both the rejected pair and exact valid alternatives are needed for an immediate correction.
	for _, expected := range []string{`"code":"auth_selection_not_found"`, `auth_type=\"basic\"`, `auth_name=\"oauth2\"`, `auth_type=\"api_key\"`, `auth_name=\"providerKey\"`, `auth_type=\"oauth\"`, `auth_name=\"googleOAuth\"`} {
		if !strings.Contains(message, expected) {
			t.Fatalf("selector error %s omitted %q", message, expected)
		}
	}
}

// TestFormatSelectionsBoundsAndSanitizes verifies provider metadata cannot leak through copyable suggestions.
func TestFormatSelectionsBoundsAndSanitizes(t *testing.T) {
	selections := []Selection{
		{AuthType: "oauth", AuthName: "safe"},
		{AuthType: "oauth", AuthName: "safe"},
		{AuthType: "api_key", AuthName: "password=hidden"},
		{AuthType: "basic", AuthName: "b"},
		{AuthType: "bearer", AuthName: "c"},
		{AuthType: "digest", AuthName: "d"},
		{AuthType: "mtls", AuthName: "e"},
		{AuthType: "oidc", AuthName: "f"},
	}
	formatted := FormatSelections(selections, "auth.type", "auth.name")
	// Distinct safe choices are capped, while duplicates and credential-shaped names are omitted.
	if strings.Count(formatted, "auth.type=") != SuggestionLimit || !strings.Contains(formatted, "additional selections omitted") || strings.Contains(formatted, "hidden") {
		t.Fatalf("formatted selections = %q", formatted)
	}
}
