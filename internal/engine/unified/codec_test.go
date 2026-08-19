package unified

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// TestProgramCodecRoundTripIsDeterministic protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestProgramCodecRoundTripIsDeterministic(t *testing.T) {
	program, err := CompileWithTargets(map[string]any{
		"id":       "${response.@acme/custom-crm.id ?? response.github.id}",
		"mode":     "general",
		"optional": "${input.note?}",
		"target":   "${target}",
		"items":    []any{true, 3},
	}, DefaultLimits(), []string{"github", "@acme/custom-crm"})
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}

	first, err := EncodeProgram(program, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeProgram() error = %v", err)
	}
	restored, err := DecodeProgram(first, DefaultLimits(), []string{"@acme/custom-crm", "github"})
	if err != nil {
		t.Fatalf("DecodeProgram() error = %v", err)
	}
	second, err := EncodeProgram(restored, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeProgram(restored) error = %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("codec is not deterministic:\n%s\n%s", first, second)
	}
	before, err := program.Evaluate(EvaluationContext{
		Target: "@acme/custom-crm", Response: map[string]any{"id": "crm-1"}, Input: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Evaluate(original) error = %v", err)
	}

	got, err := restored.Evaluate(EvaluationContext{
		Target: "@acme/custom-crm", Response: map[string]any{"id": "crm-1"}, Input: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	want := map[string]any{
		"id": "crm-1", "mode": "general", "target": "@acme/custom-crm",
		"items": []any{true, json.Number("3")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, want %#v", got, want)
	}
	if !reflect.DeepEqual(got, before) {
		t.Fatalf("round trip changed evaluation: before %#v, after %#v", before, got)
	}
}

// TestProgramCodecPreservesEmptyComposites protects the rule that empty objects and arrays remain distinct valid JSON values through persistence.
func TestProgramCodecPreservesEmptyComposites(t *testing.T) {
	program := mustCompile(t, map[string]any{"object": map[string]any{}, "array": []any{}})
	encoded, err := EncodeProgram(program, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeProgram() error = %v", err)
	}
	restored, err := DecodeProgram(encoded, DefaultLimits(), nil)
	if err != nil {
		t.Fatalf("DecodeProgram() error = %v", err)
	}
	got, err := restored.Evaluate(EvaluationContext{})
	want := map[string]any{"object": map[string]any{}, "array": []any{}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, %v, want %#v", got, err, want)
	}
}

// TestDecodeProgramRejectsCorruptionAndUnknownVersion ensures persisted
// bytecode cannot add fields, duplicate keys, or opt into an unknown schema.
func TestDecodeProgramRejectsCorruptionAndUnknownVersion(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"schema_version":2,"root":{"kind":"literal","literal":true}}`),
		[]byte(`{"schema_version":1,"root":{"kind":"literal","literal":true},"unknown":1}`),
		[]byte(`{"schema_version":1,"root":{"kind":"literal","literal":true,"items":[]}}`),
		[]byte(`{"schema_version":1,"root":{"kind":"reference","references":[]}}`),
		[]byte(`{"schema_version":1,"root":{"kind":"literal","literal":{}}}`),
		[]byte(`{"schema_version":1,"root":{"kind":"literal","literal":true}} trailing`),
	}
	for _, encoded := range tests {
		if _, err := DecodeProgram(encoded, DefaultLimits(), nil); !errors.Is(err, ErrInvalidProgram) {
			t.Errorf("DecodeProgram(%s) error = %v, want ErrInvalidProgram", encoded, err)
		}
	}
}

// TestDecodeProgramRevalidatesAllowedTargets proves persisted bytecode cannot
// forge a response namespace excluded from the containing binding.
func TestDecodeProgramRevalidatesAllowedTargets(t *testing.T) {
	program, err := CompileWithTargets("${response.@acme/custom-crm.id}", DefaultLimits(), []string{"@acme/custom-crm"})
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	encoded, err := EncodeProgram(program, DefaultLimits())
	if err != nil {
		t.Fatalf("EncodeProgram() error = %v", err)
	}
	_, err = DecodeProgram(encoded, DefaultLimits(), []string{"github"})
	if !errors.Is(err, ErrInvalidProgram) {
		t.Fatalf("DecodeProgram() error = %v, want ErrInvalidProgram", err)
	}
}

// TestProgramCodecEnforcesBounds protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestProgramCodecEnforcesBounds(t *testing.T) {
	tests := []struct {
		name    string
		program *Program
		limit   func(*Limits, int)
	}{
		{
			name: "depth", program: mustCompile(t, map[string]any{"nested": map[string]any{"value": true}}),
			limit: func(value *Limits, _ int) { value.MaxDepth = 1 },
		},
		{
			name: "nodes", program: mustCompile(t, []any{true, false}),
			limit: func(value *Limits, _ int) { value.MaxNodes = 2 },
		},
		{
			name: "expressions", program: mustCompile(t, []any{"${input.a}", "${input.b}"}),
			limit: func(value *Limits, _ int) { value.MaxExpressions = 1 },
		},
		{
			name: "path", program: mustCompile(t, "${input.a.b}"),
			limit: func(value *Limits, _ int) { value.MaxPathSegments = 1 },
		},
		{
			name: "expression length", program: mustCompile(t, "${input.a ?? input.b}"),
			limit: func(value *Limits, _ int) { value.MaxExpressionLength = 12 },
		},
		{
			name: "encoded size", program: mustCompile(t, map[string]any{"value": true}),
			limit: func(value *Limits, encodedSize int) { value.MaxEncodedBytes = encodedSize - 1 },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := EncodeProgram(test.program, DefaultLimits())
			if err != nil {
				t.Fatalf("EncodeProgram() error = %v", err)
			}
			limited := DefaultLimits()
			test.limit(&limited, len(encoded))
			if _, err := EncodeProgram(test.program, limited); !errors.Is(err, ErrLimitExceeded) {
				t.Errorf("EncodeProgram(limited) error = %v, want ErrLimitExceeded", err)
			}
			if _, err := DecodeProgram(encoded, limited, nil); !errors.Is(err, ErrLimitExceeded) {
				t.Errorf("DecodeProgram(limited) error = %v, want ErrLimitExceeded", err)
			}
		})
	}
}

// TestEncodeProgramRejectsUnsupportedLiteral keeps non-JSON Go values out of
// the deterministic private bytecode format.
func TestEncodeProgramRejectsUnsupportedLiteral(t *testing.T) {
	program := &Program{root: literalNode{value: struct{}{}}}
	_, err := EncodeProgram(program, DefaultLimits())
	if !errors.Is(err, ErrInvalidProgram) {
		t.Fatalf("EncodeProgram() error = %v, want ErrInvalidProgram", err)
	}
}
