package store

import "testing"

func TestNormalizeArtifactKind(t *testing.T) {
	for input, want := range map[string]string{"": "", " SDK ": "sdk", "MCP": "mcp"} {
		got, ok := normalizeArtifactKind(input)
		if !ok || got != want {
			t.Fatalf("normalizeArtifactKind(%q) = %q, %t; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := normalizeArtifactKind("webhook"); ok {
		t.Fatal("webhook must not be accepted as an SDK/MCP artifact kind")
	}
}
