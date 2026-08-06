package canonical

import (
	"testing"
)

func TestAppName(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		wantCanonical string
		wantDisplay   string
		wantErr       bool
		errContains   string
	}{
		// Basic identity
		{name: "simple ascii", raw: "MySdk", wantCanonical: "mysdk", wantDisplay: "MySdk"},
		{name: "already lowercase", raw: "mysdk", wantCanonical: "mysdk", wantDisplay: "mysdk"},

		// Whitespace trimming
		{name: "leading space", raw: "  mysdk", wantCanonical: "mysdk", wantDisplay: "mysdk"},
		{name: "trailing space", raw: "mysdk  ", wantCanonical: "mysdk", wantDisplay: "mysdk"},
		{name: "surrounding spaces", raw: "  mysdk  ", wantCanonical: "mysdk", wantDisplay: "mysdk"},
		{name: "unicode nbsp", raw: "\u00a0mysdk\u00a0", wantCanonical: "mysdk", wantDisplay: "mysdk"},

		// Unicode NFC normalization — the canonical form must be identical
		// for precomposed and decomposed representations.
		{name: "nfc precomposed é", raw: "café", wantCanonical: "café", wantDisplay: "café"},
		{name: "nfd decomposed é", raw: "cafe\u0301", wantCanonical: "café", wantDisplay: "café"},

		// Case folding
		{name: "mixed case", raw: "My-SDK-Name", wantCanonical: "my-sdk-name", wantDisplay: "My-SDK-Name"},
		{name: "uppercase", raw: "MYSDK", wantCanonical: "mysdk", wantDisplay: "MYSDK"},

		// Colons are preserved — names may contain them
		{name: "contains colon", raw: "jira:plunk-sdk", wantCanonical: "jira:plunk-sdk", wantDisplay: "jira:plunk-sdk"},
		{name: "multiple colons", raw: "a:b:c", wantCanonical: "a:b:c", wantDisplay: "a:b:c"},

		// Punctuation preserved
		{name: "hyphens and dots", raw: "my-sdk.v2", wantCanonical: "my-sdk.v2", wantDisplay: "my-sdk.v2"},
		{name: "underscores", raw: "my_sdk_name", wantCanonical: "my_sdk_name", wantDisplay: "my_sdk_name"},

		// Empty and whitespace-only
		{name: "empty string", raw: "", wantErr: true, errContains: "empty"},
		{name: "whitespace only", raw: "   ", wantErr: true, errContains: "whitespace"},
		{name: "unicode whitespace only", raw: "\u00a0\u2003", wantErr: true, errContains: "whitespace"},

		// Control characters are rejected (except common whitespace which is trimmed)
		{name: "null byte", raw: "bad\x00name", wantErr: true, errContains: "control"},
		{name: "bell char", raw: "bad\aname", wantErr: true, errContains: "control"},
		{name: "embedded newline", raw: "bad\nname", wantErr: true, errContains: "control"},
		{name: "embedded tab", raw: "bad\tname", wantErr: true, errContains: "control"},
		{name: "unicode case fold", raw: "Straße", wantCanonical: "strasse", wantDisplay: "Straße"},

		// Idempotency — normalizing twice yields the same result
		{name: "idempotent", raw: "  Café  ", wantCanonical: "café", wantDisplay: "Café"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCanonical, gotDisplay, err := AppName(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("AppName(%q) expected error, got none", tt.raw)
				}
				if tt.errContains != "" {
					errStr := err.Error()
					if !contains(errStr, tt.errContains) {
						t.Fatalf("AppName(%q) error %q does not contain %q", tt.raw, errStr, tt.errContains)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("AppName(%q) unexpected error: %v", tt.raw, err)
			}
			if gotCanonical != tt.wantCanonical {
				t.Errorf("AppName(%q) canonical = %q, want %q", tt.raw, gotCanonical, tt.wantCanonical)
			}
			if gotDisplay != tt.wantDisplay {
				t.Errorf("AppName(%q) display = %q, want %q", tt.raw, gotDisplay, tt.wantDisplay)
			}

			// Verify idempotency: normalizing the canonical result again must
			// produce the same canonical form. The display name naturally
			// reflects the NFC form of the input — when feeding back the
			// already-lowercased canonical, display will match canonical.
			canonical2, display2, err2 := AppName(gotCanonical)
			if err2 != nil {
				t.Fatalf("AppName(%q) idempotent check error: %v", gotCanonical, err2)
			}
			if canonical2 != gotCanonical {
				t.Errorf("AppName not idempotent: AppName(%q).canonical = %q, want %q", gotCanonical, canonical2, gotCanonical)
			}
			// Display of the canonical form should match the canonical form
			// itself (since it's already case-folded and NFC-normalized).
			if display2 != gotCanonical {
				t.Errorf("AppName not idempotent for display: AppName(%q).display = %q, want %q",
					gotCanonical, display2, gotCanonical)
			}
		})
	}
}

// TestAppNameGrouping verifies that names that should group together produce
// the same canonical form.
func TestAppNameGrouping(t *testing.T) {
	groups := [][]string{
		{"MySdk", "mysdk", "MYSDK", "  mysdk  "},
		{"café", "cafe\u0301"}, // precomposed vs decomposed
		{"jira-sdk", "Jira-SDK", "JIRA-SDK"},
	}

	for i, group := range groups {
		var canonical string
		for j, raw := range group {
			c, _, err := AppName(raw)
			if err != nil {
				t.Fatalf("group %d name %d AppName(%q): %v", i, j, raw, err)
			}
			if j == 0 {
				canonical = c
			} else if c != canonical {
				t.Errorf("group %d: AppName(%q) = %q, want %q (matching first entry %q)",
					i, raw, c, canonical, group[0])
			}
		}
	}
}

// TestAppNameSeparation verifies that names that should NOT group together
// produce different canonical forms.
func TestAppNameSeparation(t *testing.T) {
	pairs := [][2]string{
		{"abc", "abcd"},          // different lengths
		{"my-sdk", "my-sdk-v2"},  // different version-like suffixes
		{"sdk-a", "sdk-b"},       // different names
		{"jira-sdk", "jira-mcp"}, // name includes kind-like suffix
	}

	for _, pair := range pairs {
		c1, _, err1 := AppName(pair[0])
		if err1 != nil {
			t.Fatalf("AppName(%q): %v", pair[0], err1)
		}
		c2, _, err2 := AppName(pair[1])
		if err2 != nil {
			t.Fatalf("AppName(%q): %v", pair[1], err2)
		}
		if c1 == c2 {
			t.Errorf("AppName(%q) and AppName(%q) both produced %q — should differ",
				pair[0], pair[1], c1)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
