package store

import "testing"

func TestNormalizeAppKind(t *testing.T) {
	for input, want := range map[string]string{"": "", " SDK ": "sdk", "MCP": "mcp"} {
		got, ok := normalizeAppKind(input)
		if !ok || got != want {
			t.Fatalf("normalizeAppKind(%q) = %q, %t; want %q, true", input, got, ok, want)
		}
	}
	if _, ok := normalizeAppKind("webhook"); ok {
		t.Fatal("webhook must not be accepted as an SDK/MCP artifact kind")
	}
}
