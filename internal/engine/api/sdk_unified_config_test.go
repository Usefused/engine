package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDecodeSDKUnifiedOperationsAcceptsCompactAndExpandedBindings protects the rule that binding shorthand and expanded objects compile to one canonical contract.
func TestDecodeSDKUnifiedOperationsAcceptsCompactAndExpandedBindings(t *testing.T) {
	doc := decodeUnifiedDocument(t, `{
		"github":"createIssue",
		"@acme/custom-crm":{"operation":"meta/get","input":{"title":"${input.title}"}}
	}`, `null`, "typescript")
	if err := validateSDKConfigDocument(doc); err != nil {
		t.Fatalf("validateSDKConfigDocument() error = %v", err)
	}
	if doc.UnifiedOperations["issues.create"].Bindings["github"].Operation != "createIssue" {
		t.Fatal("compact binding did not preserve operationId")
	}
}

// TestValidateSDKUnifiedOperationsAcceptsServiceAliases proves graph step names
// can differ from the selected service without widening the service selection.
func TestValidateSDKUnifiedOperationsAcceptsServiceAliases(t *testing.T) {
	doc := decodeUnifiedDocument(t, `{
		"github_lookup":{"service":"github","operation":"createIssue"},
		"github":{"operation":"createIssue","depends_on":["github_lookup"]}
	}`, `null`, "typescript")
	if err := validateSDKConfigDocument(doc); err != nil {
		t.Fatalf("validateSDKConfigDocument() error = %v", err)
	}
	alias := doc.UnifiedOperations["issues.create"].Bindings["github_lookup"]
	if alias.Service != "github" {
		t.Fatalf("canonical service = %q, want github", alias.Service)
	}
}

// TestValidateSDKUnifiedOperationsRejectsUnknownAliasedService keeps an alias
// from escaping the SDK's explicitly selected service set.
func TestValidateSDKUnifiedOperationsRejectsUnknownAliasedService(t *testing.T) {
	doc := decodeUnifiedDocument(t, `{"github_lookup":{"service":"missing","operation":"createIssue"}}`, `null`, "typescript")
	err := validateSDKConfigDocument(doc)
	if err == nil || !strings.Contains(err.Error(), "configured service") {
		t.Fatalf("validateSDKConfigDocument() error = %v, want configured service", err)
	}
}

// TestValidateSDKUnifiedOperationsRejectsInvalidShapes covers unresolved
// services/operations, competing output modes, and unsupported SDK languages.
func TestValidateSDKUnifiedOperationsRejectsInvalidShapes(t *testing.T) {
	tests := []struct {
		name     string
		bindings string
		output   string
		language string
		want     string
	}{
		{name: "missing service", bindings: `{"missing":"createIssue"}`, output: `null`, language: "typescript", want: "configured service"},
		{name: "unselected operation", bindings: `{"github":"deleteIssue"}`, output: `null`, language: "typescript", want: "is not selected"},
		{name: "root and binding output", bindings: `{"github":{"operation":"createIssue","output":{"schema":{"type":"object"},"mapping":{"id":"${response.github.id}"}}}}`, output: `{"schema":{"type":"object"},"mapping":{"id":"${response.github.id}"}}`, language: "typescript", want: "cannot combine"},
		{name: "unsupported language", bindings: `{"github":"createIssue"}`, output: `null`, language: "go", want: "TypeScript or Python"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := decodeUnifiedDocument(t, test.bindings, test.output, test.language)
			err := validateSDKConfigDocument(doc)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSDKConfigDocument() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

// TestDecodeSDKUnifiedBindingRejectsUnknownFields prevents unsupported fallback
// keys from being silently ignored by strict configuration decoding.
func TestDecodeSDKUnifiedBindingRejectsUnknownFields(t *testing.T) {
	raw := []byte(`{"operation":"createIssue","fallback":"other"}`)
	var binding sdkUnifiedBindingDoc
	if err := json.Unmarshal(raw, &binding); err == nil {
		t.Fatal("binding accepted an unknown field")
	}
}

// TestValidateSDKUnifiedDependenciesAndRollback covers accepted graph syntax
// plus fail-closed missing, duplicate, self, cycle, and unselected operations.
func TestValidateSDKUnifiedDependenciesAndRollback(t *testing.T) {
	valid := decodeUnifiedDocument(t, `{
		"github":{"operation":"createIssue","depends_on":["@acme/custom-crm"]},
		"@acme/custom-crm":{"operation":"meta/get","rollback":{"operation":"meta/get","input":{"id":"${response.@acme/custom-crm.id}"}}}
	}`, `null`, "typescript")
	if err := validateSDKConfigDocument(valid); err != nil {
		t.Fatalf("valid dependency config: %v", err)
	}
	tests := []struct {
		name     string
		bindings string
		want     string
	}{
		{name: "missing", bindings: `{"github":{"operation":"createIssue","depends_on":["missing"]}}`, want: "not bound"},
		{name: "self", bindings: `{"github":{"operation":"createIssue","depends_on":["github"]}}`, want: "itself"},
		{name: "duplicate", bindings: `{"github":{"operation":"createIssue","depends_on":["@acme/custom-crm","@acme/custom-crm"]},"@acme/custom-crm":"meta/get"}`, want: "unique"},
		{name: "cycle", bindings: `{"github":{"operation":"createIssue","depends_on":["@acme/custom-crm"]},"@acme/custom-crm":{"operation":"meta/get","depends_on":["github"]}}`, want: "cycle"},
		{name: "rollback unselected", bindings: `{"github":{"operation":"createIssue","rollback":{"operation":"deleteIssue"}}}`, want: "rollback operation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := decodeUnifiedDocument(t, test.bindings, `null`, "typescript")
			err := validateSDKConfigDocument(doc)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validation error = %v, want %q", err, test.want)
			}
		})
	}
}

// TestValidateSDKUnifiedOperationsRejectsGeneratedNamespaceCollision prevents
// two operation paths from generating the same language namespace.
func TestValidateSDKUnifiedOperationsRejectsGeneratedNamespaceCollision(t *testing.T) {
	doc := decodeUnifiedDocument(t, `{"github":"createIssue"}`, `null`, "python")
	doc.UnifiedOperations["issues"] = doc.UnifiedOperations["issues.create"]
	err := validateSDKConfigDocument(doc)
	if err == nil || !strings.Contains(err.Error(), "collide as generated namespace paths") {
		t.Fatalf("validateSDKConfigDocument() error = %v", err)
	}
}

// TestValidateSDKUnifiedOperationsRejectsUnsafeGeneratedNames covers
// normalization collisions and language keywords before source generation.
func TestValidateSDKUnifiedOperationsRejectsUnsafeGeneratedNames(t *testing.T) {
	tests := []struct {
		name      string
		language  string
		operation string
		want      string
	}{
		{name: "normalized type", language: "typescript", operation: "foo.bar_one", want: "generated type names"},
		{name: "normalized namespace", language: "typescript", operation: "foo.bar.y", want: "collide after code generation"},
		{name: "Python keyword", language: "python", operation: "issues.class", want: "Python keyword"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			doc := decodeUnifiedDocument(t, `{"github":"createIssue"}`, `null`, test.language)
			base := doc.UnifiedOperations["issues.create"]
			delete(doc.UnifiedOperations, "issues.create")
			if test.name == "normalized type" {
				doc.UnifiedOperations["foo_bar.one"] = base
			}
			if test.name == "normalized namespace" {
				doc.UnifiedOperations["foo_bar.x"] = base
			}
			doc.UnifiedOperations[test.operation] = base
			err := validateSDKConfigDocument(doc)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateSDKConfigDocument() error = %v, want containing %q", err, test.want)
			}
		})
	}
}

// TestCanonicalAppStateNormalizesUnifiedJSONMembers protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestCanonicalAppStateNormalizesUnifiedJSONMembers(t *testing.T) {
	first := decodeUnifiedDocument(t, `{"github":{"operation":"createIssue","input":{"b":2,"a":1}}}`, `null`, "typescript")
	second := decodeUnifiedDocument(t, `{"github":{"input":{"a":1,"b":2},"operation":"createIssue"}}`, `null`, "typescript")
	left, err := canonicalAppState(first)
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalAppState(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(left) != string(right) {
		t.Fatalf("canonical Unified state differs:\n%s\n%s", left, right)
	}
}

// decodeUnifiedDocument embeds one binding/output fragment in a complete SDK
// document and fails the test at the strict JSON decoding boundary.
func decodeUnifiedDocument(t *testing.T, bindings, output, language string) sdkConfigDocument {
	t.Helper()
	raw := `{
		"apiVersion":"fused/v1","kind":"sdk","name":"engineering","version":"1.0.0",
		"language":` + quoteJSON(language) + `,"bucket":"default",
		"services":{
			"github":{"version":"v1","operations":["createIssue"]},
			"@acme/custom-crm":{"version":"v1","operations":["meta/get"]}
		},
		"unified_operations":{
			"issues.create":{
				"input":{"type":"object","properties":{"title":{"type":"string"}}},
				"bindings":` + bindings + `,
				"output":` + output + `
			}
		}
	}`
	var doc sdkConfigDocument
	if err := decodeAppConfigJSON([]byte(raw), &doc); err != nil {
		t.Fatalf("decodeAppConfigJSON() error = %v", err)
	}
	return doc
}

// quoteJSON safely embeds language fixture values in hand-built configuration JSON.
func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
