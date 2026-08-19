package unified

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var definitionTestIDs = [3]uuid.UUID{
	uuid.MustParse("11111111-1111-4111-8111-111111111111"),
	uuid.MustParse("22222222-2222-4222-8222-222222222222"),
	uuid.MustParse("33333333-3333-4333-8333-333333333333"),
}

// TestDefinitionsCodecRoundTripIsCanonicalAndExecutable protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestDefinitionsCodecRoundTripIsCanonicalAndExecutable(t *testing.T) {
	input := mustCompile(t, map[string]any{"title": "${input.title}"})
	mapping, err := CompileWithTargets(
		map[string]any{"id": "${response.@acme/custom-crm.id ?? response.github.id}"},
		DefaultLimits(), []string{"github", "@acme/custom-crm"},
	)
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	definitions := []OperationDefinition{
		definitionFixture("zeta.run", "github", input, nil),
		{
			Name: "issues.create", Description: "Create an issue",
			InputSchema: []byte(`{ "type": "object", "properties": {"title":{"type":"string"}} }`),
			Bindings: []BindingDefinition{
				bindingFixture("github", input),
				bindingFixture("@acme/custom-crm", input),
			},
			Output: &OutputDefinition{Schema: []byte(`{"type":"object"}`), Mapping: mapping},
		},
	}

	first := mustEncodeDefinitions(t, definitions)
	restored := mustDecodeDefinitions(t, first)
	second := mustEncodeDefinitions(t, restored)
	if !bytes.Equal(first, second) {
		t.Fatalf("definition codec is not deterministic:\n%s\n%s", first, second)
	}
	if len(restored) != 2 || restored[0].Name != "issues.create" || restored[0].Bindings[0].PublicTarget != "@acme/custom-crm" {
		t.Fatalf("definitions were not canonically ordered: %#v", restored)
	}
	got, err := restored[0].Output.Mapping.Evaluate(EvaluationContext{
		Target: "@acme/custom-crm", Response: map[string]any{"id": "crm-1"},
	})
	if err != nil || got.(map[string]any)["id"] != "crm-1" {
		t.Fatalf("Evaluate() = %#v, %v, want crm-1", got, err)
	}
}

// TestDefinitionsCodecPreservesServiceTargetAndDefaultsPublicTarget protects
// both explicit and contract-default selector routing.
func TestDefinitionsCodecPreservesServiceTargetAndDefaultsPublicTarget(t *testing.T) {
	definition := definitionFixture("issues.create", "jira_projects", nil, nil)
	definition.Bindings[0].ServiceTarget = "jira"
	restored := mustDecodeDefinitions(t, mustEncodeDefinitions(t, []OperationDefinition{definition}))
	if restored[0].Bindings[0].ServiceTarget != "jira" {
		t.Fatalf("service target = %q, want jira", restored[0].Bindings[0].ServiceTarget)
	}
	defaulted := []byte(`[` + validWireOperation(validWireBinding(`{"schema_version":1,"root":{"kind":"literal","literal":true}}`)) + `]`)
	restored = mustDecodeDefinitions(t, defaulted)
	if restored[0].Bindings[0].ServiceTarget != restored[0].Bindings[0].PublicTarget {
		t.Fatalf("defaulted binding = %#v", restored[0].Bindings[0])
	}
}

// TestDefinitionsCodecRejectsInvalidServiceTarget prevents persisted selector
// namespaces from using ambiguous surrounding whitespace.
func TestDefinitionsCodecRejectsInvalidServiceTarget(t *testing.T) {
	definition := definitionFixture("issues.create", "jira_projects", nil, nil)
	definition.Bindings[0].ServiceTarget = " jira "
	if _, err := EncodeDefinitions([]OperationDefinition{definition}, DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("EncodeDefinitions() error = %v, want ErrInvalidDefinitions", err)
	}
}

// TestDefinitionsCodecPreservesCanonicalEmptySet protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestDefinitionsCodecPreservesCanonicalEmptySet(t *testing.T) {
	encoded, err := EncodeDefinitions(nil, DefaultLimits())
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("EncodeDefinitions(nil) = %s, %v, want []", encoded, err)
	}
	decoded, err := DecodeDefinitions(encoded, DefaultLimits())
	if err != nil || decoded == nil || len(decoded) != 0 {
		t.Fatalf("DecodeDefinitions([]) = %#v, %v, want non-nil empty slice", decoded, err)
	}
}

// TestDecodeDefinitionsRejectsMalformedPrivateData covers strict structural,
// identity, program-version, duplicate, and output-mode admission.
func TestDecodeDefinitionsRejectsMalformedPrivateData(t *testing.T) {
	binding := validWireBinding(`{"schema_version":1,"root":{"kind":"literal","literal":true}}`)
	operation := validWireOperation(binding)
	output := `{"schema":{"type":"object"},"mapping":{"schema_version":1,"root":{"kind":"literal","literal":true}}}`
	tests := map[string]string{
		"not array":            `{}`,
		"unknown field":        `[{"unknown":true,` + strings.TrimPrefix(operation, `{`),
		"duplicate JSON field": `[{$NAME,"name":"issues.other",` + strings.TrimPrefix(operation, `{"name":"issues.create",`),
		"duplicate operation":  `[` + operation + `,` + operation + `]`,
		"duplicate target":     `[` + validWireOperation(binding+`,`+binding) + `]`,
		"missing binding":      `[{"name":"issues.create","input_schema":{},"bindings":[]}]`,
		"invalid UUID":         `[` + strings.Replace(operation, definitionTestIDs[0].String(), "not-a-uuid", 1) + `]`,
		"non-object schema":    `[` + strings.Replace(operation, `"input_schema":{"type":"object"}`, `"input_schema":[]`, 1) + `]`,
		"program version":      `[` + strings.Replace(operation, `"schema_version":1`, `"schema_version":2`, 1) + `]`,
		"missing mapping":      `[{"name":"issues.create","input_schema":{},"bindings":[` + binding + `],"output":{"schema":{}}}]`,
		"mixed outputs":        `[{"name":"issues.create","input_schema":{},"bindings":[` + strings.TrimSuffix(binding, `}`) + `,"output":` + output + `}],"output":` + output + `}]`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			raw = strings.Replace(raw, `$NAME`, `"name":"issues.create"`, 1)
			if _, err := DecodeDefinitions([]byte(raw), DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
				t.Fatalf("DecodeDefinitions(%s) error = %v, want ErrInvalidDefinitions", raw, err)
			}
		})
	}
}

// TestDefinitionsCodecRejectsResponseReferenceInBindingInput proves a forward
// mapping cannot read response data without a declared dependency edge.
func TestDefinitionsCodecRejectsResponseReferenceInBindingInput(t *testing.T) {
	program, err := CompileWithTargets("${response.github.id}", DefaultLimits(), []string{"github"})
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	definition := definitionFixture("issues.create", "github", program, nil)
	if _, err := EncodeDefinitions([]OperationDefinition{definition}, DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("EncodeDefinitions() error = %v, want ErrInvalidDefinitions", err)
	}

	wireProgram := `{"schema_version":1,"root":{"kind":"reference","references":[{"source":"response","service":"github","path":["id"]}]}}`
	raw := `[` + validWireOperation(validWireBinding(wireProgram)) + `]`
	if _, err := DecodeDefinitions([]byte(raw), DefaultLimits()); !errors.Is(err, ErrInvalidDefinitions) {
		t.Fatalf("DecodeDefinitions() error = %v, want ErrInvalidDefinitions", err)
	}
}

// TestDefinitionsCodecAllowsBoundResponseReferenceInOutput protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestDefinitionsCodecAllowsBoundResponseReferenceInOutput(t *testing.T) {
	mapping, err := CompileWithTargets("${response.github.id}", DefaultLimits(), []string{"github"})
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	definition := definitionFixture("issues.create", "github", nil, &OutputDefinition{
		Schema: []byte(`{"type":"string"}`), Mapping: mapping,
	})
	restored := mustDecodeDefinitions(t, mustEncodeDefinitions(t, []OperationDefinition{definition}))
	got, err := restored[0].Output.Mapping.Evaluate(EvaluationContext{
		Target: "github", Response: map[string]any{"id": "issue-1"},
	})
	if err != nil || got != "issue-1" {
		t.Fatalf("Evaluate() = %#v, %v, want issue-1", got, err)
	}
}

// TestDefinitionsCodecEnforcesDefinitionBounds protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestDefinitionsCodecEnforcesDefinitionBounds(t *testing.T) {
	program := mustCompile(t, true)
	definitions := make([]OperationDefinition, maxDefinitionOperations+1)
	for index := range definitions {
		definitions[index] = definitionFixture(fmt.Sprintf("operation%d", index), "github", program, nil)
	}
	if _, err := EncodeDefinitions(definitions, DefaultLimits()); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("EncodeDefinitions(operations) error = %v, want ErrLimitExceeded", err)
	}

	bindings := make([]BindingDefinition, maxDefinitionBindings+1)
	for index := range bindings {
		bindings[index] = bindingFixture(fmt.Sprintf("target%d", index), program)
	}
	tooManyBindings := definitionFixture("issues.create", "unused", program, nil)
	tooManyBindings.Bindings = bindings
	if _, err := EncodeDefinitions([]OperationDefinition{tooManyBindings}, DefaultLimits()); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("EncodeDefinitions(bindings) error = %v, want ErrLimitExceeded", err)
	}

	valid, err := EncodeDefinitions([]OperationDefinition{definitionFixture("issues.create", "github", program, nil)}, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeDefinitions() error = %v", err)
	}
	limited := DefaultLimits()
	limited.MaxEncodedBytes = len(valid) - 1
	if _, err := DecodeDefinitions(valid, limited); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("DecodeDefinitions(size) error = %v, want ErrLimitExceeded", err)
	}
}

// bindingFixture supplies deterministic immutable Unified definitions inputs without bypassing the production boundary.
func bindingFixture(target string, input *Program) BindingDefinition {
	return BindingDefinition{
		PublicTarget: target, OperationID: "createIssue",
		ServiceID: definitionTestIDs[0], ServiceVersionID: definitionTestIDs[1],
		EndpointID: definitionTestIDs[2], Input: input,
	}
}

// definitionFixture supplies deterministic immutable Unified definitions inputs without bypassing the production boundary.
func definitionFixture(name, target string, input *Program, output *OutputDefinition) OperationDefinition {
	return OperationDefinition{
		Name: name, InputSchema: []byte(`{"type":"object"}`),
		Bindings: []BindingDefinition{bindingFixture(target, input)}, Output: output,
	}
}

// mustEncodeDefinitions stops fixture construction immediately when immutable Unified definitions setup is invalid.
func mustEncodeDefinitions(t *testing.T, definitions []OperationDefinition) []byte {
	t.Helper()
	encoded, err := EncodeDefinitions(definitions, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeDefinitions() error = %v", err)
	}
	return encoded
}

// mustDecodeDefinitions stops fixture construction immediately when immutable Unified definitions setup is invalid.
func mustDecodeDefinitions(t *testing.T, encoded []byte) []OperationDefinition {
	t.Helper()
	definitions, err := DecodeDefinitions(encoded, DefaultLimits())
	if err != nil {
		t.Fatalf("DecodeDefinitions() error = %v", err)
	}
	return definitions
}

// validWireBinding returns a minimal well-formed persisted binding that tests
// mutate one field at a time to isolate strict-decoder failures.
func validWireBinding(program string) string {
	return fmt.Sprintf(
		`{"public_target":"github","operation_id":"createIssue","service_id":"%s","service_version_id":"%s","endpoint_id":"%s","input":%s}`,
		definitionTestIDs[0], definitionTestIDs[1], definitionTestIDs[2], program,
	)
}

// validWireOperation wraps the valid binding in a complete persisted operation
// so corruption cases start from the same admitted baseline.
func validWireOperation(bindings string) string {
	return `{"name":"issues.create","input_schema":{"type":"object"},"bindings":[` + bindings + `]}`
}
