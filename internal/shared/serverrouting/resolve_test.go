package serverrouting

import "testing"

// TestIsVariableName keeps configuration admission on the runtime placeholder grammar.
func TestIsVariableName(t *testing.T) {
	// The accepted name is portable while slash would change URL structure.
	if !IsVariableName("app_id") || IsVariableName("app/id") {
		t.Fatal("unexpected server variable name admission")
	}
}

// TestValidateResolvedHostAnchor covers provider-anchored, path-only, and
// arbitrary-host templates at the shared routing boundary.
func TestValidateResolvedHostAnchor(t *testing.T) {
	tests := []struct {
		name     string
		template string
		resolved string
		supplied map[string]bool
		wantErr  bool
	}{
		{name: "Sendbird anchor", template: "https://api-{app_id}.sendbird.com", resolved: "https://api-sandbox-123.sendbird.com", supplied: map[string]bool{"app_id": true}},
		{name: "path only", template: "https://api.example.com/{tenant}", resolved: "https://api.example.com/acme", supplied: map[string]bool{"tenant": true}},
		{name: "whole host", template: "https://{host}", resolved: "https://evil.example", supplied: map[string]bool{"host": true}, wantErr: true},
		{name: "protocol-relative whole host", template: "//{host}", resolved: "https://evil.example", supplied: map[string]bool{"host": true}, wantErr: true},
		{name: "public suffix", template: "https://{tenant}.com", resolved: "https://evil.com", supplied: map[string]bool{"tenant": true}, wantErr: true},
		{name: "private suffix", template: "https://{tenant}.github.io", resolved: "https://evil.github.io", supplied: map[string]bool{"tenant": true}, wantErr: true},
	}
	// Table coverage keeps every origin decision on the same shared function.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateResolvedHostAnchor(test.template, test.resolved, test.supplied)
			// Error presence is the contract; messages intentionally omit host data.
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateResolvedHostAnchor error = %v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestResolveUsesDefaultForRequiredVariable(t *testing.T) {
	value := "api"
	resolved, dynamic, err := Resolve("https://{tenant}.example.com", []Variable{{Name: "tenant", Default: &value, Required: true}}, nil)
	if err != nil || dynamic || resolved != "https://api.example.com" {
		t.Fatalf("resolved=%q dynamic=%v err=%v", resolved, dynamic, err)
	}
}

func TestResolveUsesValidatedSuppliedValue(t *testing.T) {
	value := "api"
	variables := []Variable{{Name: "tenant", Default: &value, Enum: []string{"api", "acme"}, Required: true}}
	resolved, dynamic, err := Resolve("https://{tenant}.example.com", variables, map[string]string{"tenant": "acme"})
	if err != nil || !dynamic || resolved != "https://acme.example.com" {
		t.Fatalf("resolved=%q dynamic=%v err=%v", resolved, dynamic, err)
	}
}

func TestResolveRejectsUnresolvedUnsafeAndNonHTTPS(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		variables []Variable
		supplied  map[string]string
	}{
		{name: "unresolved", template: "https://{tenant}.example.com", variables: []Variable{{Name: "tenant", Required: true}}},
		{name: "unsafe", template: "https://{tenant}.example.com", variables: []Variable{{Name: "tenant"}}, supplied: map[string]string{"tenant": "evil/path"}},
		{name: "plain http", template: "http://{tenant}.example.com", variables: []Variable{{Name: "tenant"}}, supplied: map[string]string{"tenant": "api"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := Resolve(test.template, test.variables, test.supplied); err == nil {
				t.Fatal("expected resolution failure")
			}
		})
	}
}

func TestValidateResolvedURLAllowsHTTPSAndLoopback(t *testing.T) {
	for _, value := range []string{"https://api.example.com", "http://127.0.0.1:8080", "http://localhost:8080"} {
		if err := ValidateResolvedURL(value); err != nil {
			t.Fatalf("ValidateResolvedURL(%q): %v", value, err)
		}
	}
}
