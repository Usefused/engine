package serverrouting

import "testing"

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
