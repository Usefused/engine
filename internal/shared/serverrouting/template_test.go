package serverrouting

import (
	"strings"
	"testing"
)

// TestValidateReferenceTemplatePreservesRelativeServers prevents template fixes
// from making OpenAPI operation routes require a separately authored origin.
func TestValidateReferenceTemplatePreservesRelativeServers(t *testing.T) {
	for _, template := range []string{"/api/{version}", "../{version}", "//api.example.com/{version}", "http://localhost:8080/{version}", "https://api.example.com/{version}"} {
		t.Run(template, func(t *testing.T) {
			// The real execution boundary still binds relative references to its service.
			if err := ValidateReferenceTemplate(template, []Variable{{Name: "version", Required: true}}); err != nil {
				t.Fatalf("valid server reference rejected: %v", err)
			}
		})
	}
}

// TestValidateReferenceTemplateRejectsUnsafeDefaults exercises the same value
// and HTTPS rules used at dispatch rather than allowing provider-name bypasses.
func TestValidateReferenceTemplateRejectsUnsafeDefaults(t *testing.T) {
	tests := []struct {
		name      string
		template  string
		variables []Variable
	}{
		{name: "http default", template: "{protocol}://api.example.com", variables: []Variable{templateDefaultVariable("protocol", "http")}},
		{name: "non-http scheme", template: "{protocol}://api.example.com", variables: []Variable{templateDefaultVariable("protocol", "file")}},
		{name: "invalid port", template: "https://api.example.com:{port}", variables: []Variable{templateDefaultVariable("port", "not-a-port")}},
		{name: "userinfo literal", template: "https://user@api.example.com"},
		{name: "userinfo default", template: "https://{tenant}.example.com", variables: []Variable{templateDefaultVariable("tenant", "user@evil")}},
		{name: "path default", template: "https://{tenant}.example.com", variables: []Variable{templateDefaultVariable("tenant", "evil/path")}},
		{name: "query", template: "https://api.example.com?secret=1"},
		{name: "empty query", template: "https://api.example.com?"},
		{name: "fragment", template: "https://api.example.com#fragment"},
		{name: "undeclared", template: "https://{tenant}.example.com"},
		{name: "malformed placeholder", template: "https://{tenant/path}.example.com"},
		{name: "unresolved default", template: "https://{tenant}.example.com", variables: []Variable{templateDefaultVariable("tenant", "{other}")}},
		{name: "oversized expansion", template: "https://api.example.com/{version}", variables: []Variable{templateDefaultVariable("version", strings.Repeat("v", 2048))}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Neither template syntax nor a provider default can bypass URL admission.
			if err := ValidateReferenceTemplate(test.template, test.variables); err == nil {
				t.Fatal("unsafe server reference accepted")
			}
		})
	}
}

// TestValidateReferenceTemplateDoesNotAuthorizeRepresentatives proves that a
// successful admission cannot weaken future validation of real supplied values.
func TestValidateReferenceTemplateDoesNotAuthorizeRepresentatives(t *testing.T) {
	template := "{protocol}://{tenant}.example.com:{port}"
	variables := []Variable{{Name: "protocol", Enum: []string{"http", "https"}}, {Name: "tenant", Enum: []string{"acme", "demo"}}, {Name: "port"}}
	// Enum-bound missing values also remain valid late-bound configuration.
	if err := ValidateReferenceTemplate(template, variables); err != nil {
		t.Fatalf("deferred enum-bound template rejected: %v", err)
	}
	for _, supplied := range []map[string]string{
		{"protocol": "http", "tenant": "acme", "port": "443"},
		{"protocol": "https", "tenant": "evil", "port": "443"},
		{"protocol": "https", "tenant": "acme", "port": "443/path"},
	} {
		// Execution reuses actual values and must not see validation-only substitutes.
		if _, _, err := Resolve(template, variables, supplied); err == nil {
			t.Fatal("unsafe execution value accepted after template admission")
		}
	}
}

// templateDefaultVariable keeps test fixtures explicit without sharing mutable
// default pointers between independent routing cases.
func templateDefaultVariable(name, value string) Variable {
	return Variable{Name: name, Default: &value}
}
