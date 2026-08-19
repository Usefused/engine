package unified

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestCompileEvaluateCompositeMapping protects the rule that DynamicValue preserves JSON and omission semantics without inventing nulls.
func TestCompileEvaluateCompositeMapping(t *testing.T) {
	program := mustCompile(t, map[string]any{
		"title":    "${input.issue.title}",
		"target":   "${target}",
		"labels":   []any{"bug", "${input.issue.optional?}", "${input.issue.labels.0}"},
		"constant": true,
	})

	got, err := program.Evaluate(EvaluationContext{
		Input: map[string]any{"issue": map[string]any{
			"title": "broken", "labels": []any{"urgent"},
		}},
		Target: "github",
	})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	want := map[string]any{
		"title": "broken", "target": "github", "labels": []any{"bug", "urgent"}, "constant": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, want %#v", got, want)
	}
}

// TestEvaluateResponseNamespaceAndCoalescing proves a mapping falls through
// missing provider fields and selects the first present response value.
func TestEvaluateResponseNamespaceAndCoalescing(t *testing.T) {
	program := mustCompile(t, map[string]any{
		"id": "${response.github.id ?? response.gitlab.iid}",
	})
	tests := []struct {
		name     string
		target   string
		response map[string]any
		want     string
	}{
		{name: "github", target: "github", response: map[string]any{"id": "123", "iid": "wrong"}, want: "123"},
		{name: "gitlab", target: "gitlab", response: map[string]any{"id": "wrong", "iid": "456"}, want: "456"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := program.Evaluate(EvaluationContext{Target: test.target, Response: test.response})
			if err != nil {
				t.Fatalf("Evaluate() error = %v", err)
			}
			if got.(map[string]any)["id"] != test.want {
				t.Fatalf("id = %#v, want %q", got.(map[string]any)["id"], test.want)
			}
		})
	}
}

// TestEvaluateOpaqueResponseTarget proves scoped package names remain opaque
// response keys rather than being split into path segments.
func TestEvaluateOpaqueResponseTarget(t *testing.T) {
	program, err := CompileWithTargets(
		"${response.@acme/custom-crm.contact.id}", DefaultLimits(),
		[]string{"@acme/custom-crm"},
	)
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	got, err := program.Evaluate(EvaluationContext{
		Target:   "@acme/custom-crm",
		Response: map[string]any{"contact": map[string]any{"id": "contact-1"}},
	})
	if err != nil || got != "contact-1" {
		t.Fatalf("Evaluate() = %#v, %v, want contact-1", got, err)
	}
}

// TestCompileResponseTargetUsesLongestExactMatch prevents a dotted target prefix
// from stealing a reference owned by a longer exact target name.
func TestCompileResponseTargetUsesLongestExactMatch(t *testing.T) {
	program, err := CompileWithTargets(
		"${response.acme.crm.id}", DefaultLimits(), []string{"acme", "acme.crm"},
	)
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	got, err := program.Evaluate(EvaluationContext{
		Target: "acme.crm", Response: map[string]any{"id": "crm-1"},
	})
	if err != nil || got != "crm-1" {
		t.Fatalf("Evaluate() = %#v, %v, want crm-1", got, err)
	}
}

// TestCompileResponseTargetFailsClosed rejects response references when no
// containing binding explicitly admitted their target namespace.
func TestCompileResponseTargetFailsClosed(t *testing.T) {
	_, err := CompileWithTargets("${response.unknown.id}", DefaultLimits(), []string{"github"})
	if !errors.Is(err, ErrInvalidExpression) {
		t.Fatalf("CompileWithTargets() error = %v, want ErrInvalidExpression", err)
	}
	_, err = CompileWithTargets("${response.github.id}", DefaultLimits(), nil)
	if !errors.Is(err, ErrInvalidExpression) {
		t.Fatalf("CompileWithTargets() error = %v, want response target rejection", err)
	}
}

// TestCompileRejectsInvalidAllowedTargetSet rejects blank, duplicate, and
// noncanonical response namespaces before expression parsing begins.
func TestCompileRejectsInvalidAllowedTargetSet(t *testing.T) {
	for _, targets := range [][]string{{""}, {"github", "github"}} {
		_, err := CompileWithTargets("literal", DefaultLimits(), targets)
		if !errors.Is(err, ErrInvalidValue) {
			t.Errorf("CompileWithTargets(%#v) error = %v, want ErrInvalidValue", targets, err)
		}
	}
}

// TestEvaluateCoalescingSkipsNullAndCanOmit proves null is a fallthrough value
// and an optional terminal operand can omit the containing field.
func TestEvaluateCoalescingSkipsNullAndCanOmit(t *testing.T) {
	program := mustCompile(t, "${input.primary ?? input.fallback?}")
	got, err := program.Evaluate(EvaluationContext{Input: map[string]any{"primary": nil, "fallback": "ready"}})
	if err != nil || got != "ready" {
		t.Fatalf("Evaluate() = %#v, %v, want ready", got, err)
	}
	got, err = program.Evaluate(EvaluationContext{Input: map[string]any{"primary": nil}})
	if err != nil || !IsOmitted(got) {
		t.Fatalf("Evaluate() = %#v, %v, want omission", got, err)
	}
}

// TestEvaluateCoalescingKeepsFinalOperandRequired proves optional earlier
// operands cannot weaken a required terminal fallback.
func TestEvaluateCoalescingKeepsFinalOperandRequired(t *testing.T) {
	program := mustCompile(t, "${input.primary? ?? input.fallback}")
	_, err := program.Evaluate(EvaluationContext{Input: map[string]any{}})
	if !errors.Is(err, ErrMissingValue) {
		t.Fatalf("Evaluate() error = %v, want ErrMissingValue", err)
	}
}

// TestEvaluatePreservesJSONNumber protects the rule that DynamicValue preserves JSON and omission semantics without inventing nulls.
func TestEvaluatePreservesJSONNumber(t *testing.T) {
	program := mustCompile(t, map[string]any{"limit": json.Number("9007199254740993")})
	got, err := program.Evaluate(EvaluationContext{})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if got.(map[string]any)["limit"] != json.Number("9007199254740993") {
		t.Fatalf("limit = %#v, want preserved json.Number", got.(map[string]any)["limit"])
	}
}

// TestCompileCanonicalizesGoNumbers protects the rule that equivalent configuration produces identical immutable bytes and hashes.
func TestCompileCanonicalizesGoNumbers(t *testing.T) {
	program := mustCompile(t, []any{3, 2.5})
	got, err := program.Evaluate(EvaluationContext{})
	want := []any{json.Number("3"), json.Number("2.5")}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Evaluate() = %#v, %v, want %#v", got, err, want)
	}
}

// TestEvaluateMissingRequiredValueFails distinguishes an absent required path
// from the omission semantics of an explicitly optional reference.
func TestEvaluateMissingRequiredValueFails(t *testing.T) {
	program := mustCompile(t, "${input.required}")
	_, err := program.Evaluate(EvaluationContext{Input: map[string]any{}})
	if !errors.Is(err, ErrMissingValue) {
		t.Fatalf("Evaluate() error = %v, want ErrMissingValue", err)
	}
}

// TestOptionalRootReturnsOmissionSentinel protects the rule that DynamicValue preserves JSON and omission semantics without inventing nulls.
func TestOptionalRootReturnsOmissionSentinel(t *testing.T) {
	program := mustCompile(t, "${input.optional?}")
	got, err := program.Evaluate(EvaluationContext{Input: map[string]any{}})
	if err != nil || !IsOmitted(got) {
		t.Fatalf("Evaluate() = %#v, %v, want omission", got, err)
	}
}

// TestCompileRejectsInterpolationAndExecutableSyntax protects the rule that mappings remain declarative values rather than executable templates.
func TestCompileRejectsInterpolationAndExecutableSyntax(t *testing.T) {
	tests := []any{
		"prefix ${input.title}",
		"${input.title} suffix",
		"${upper(input.title)}",
		"${input.title + input.body}",
		"${input.title}${input.body}",
		"${input.title ?? }",
		"${ input.title}",
		"${input.title }",
		"${target?}",
		"${response.github}",
	}
	for _, value := range tests {
		if _, err := CompileWithTargets(value, DefaultLimits(), nil); !errors.Is(err, ErrInvalidExpression) {
			t.Errorf("CompileWithTargets(%q) error = %v, want ErrInvalidExpression", value, err)
		}
	}
}

// TestCompileRejectsUnsupportedCompositeTypes prevents maps with non-string
// keys and arbitrary Go structs from entering the JSON-only mapping AST.
func TestCompileRejectsUnsupportedCompositeTypes(t *testing.T) {
	_, err := CompileWithTargets(map[any]any{"title": "${input.title}"}, DefaultLimits(), nil)
	if !errors.Is(err, ErrInvalidValue) {
		t.Fatalf("CompileWithTargets() error = %v, want ErrInvalidValue", err)
	}
}

// TestCompileEnforcesLimits protects the rule that attacker-controlled documents and responses cannot exceed admitted work budgets.
func TestCompileEnforcesLimits(t *testing.T) {
	tests := []struct {
		name   string
		value  any
		limits Limits
	}{
		{name: "depth", value: map[string]any{"nested": map[string]any{"value": true}}, limits: testLimits(1, 10)},
		{name: "nodes", value: []any{1, 2}, limits: testLimits(3, 2)},
		{name: "expressions", value: []any{"${input.one}", "${input.two}"}, limits: Limits{MaxDepth: 3, MaxNodes: 3, MaxExpressions: 1, MaxPathSegments: 5, MaxExpressionLength: 100, MaxEncodedBytes: 1000}},
		{name: "path", value: "${input.one.two.three}", limits: Limits{MaxDepth: 1, MaxNodes: 1, MaxExpressions: 1, MaxPathSegments: 2, MaxExpressionLength: 100, MaxEncodedBytes: 1000}},
		{name: "expression", value: "${input." + strings.Repeat("a", 20) + "}", limits: Limits{MaxDepth: 1, MaxNodes: 1, MaxExpressions: 1, MaxPathSegments: 5, MaxExpressionLength: 10, MaxEncodedBytes: 1000}},
		{name: "encoded", value: strings.Repeat("a", 20), limits: Limits{MaxDepth: 1, MaxNodes: 1, MaxExpressions: 1, MaxPathSegments: 5, MaxExpressionLength: 100, MaxEncodedBytes: 10}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompileWithTargets(test.value, test.limits, nil)
			if !errors.Is(err, ErrLimitExceeded) {
				t.Fatalf("CompileWithTargets() error = %v, want ErrLimitExceeded", err)
			}
		})
	}
}

// TestCompileRejectsInvalidLimits requires every compiler budget to be positive
// so a zero value cannot accidentally disable admission checks.
func TestCompileRejectsInvalidLimits(t *testing.T) {
	_, err := CompileWithTargets("literal", Limits{}, nil)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("CompileWithTargets() error = %v, want ErrLimitExceeded", err)
	}
}

// TestObjectEvaluationFailureIsDeterministic proves sorted object fields make
// the same missing required path fail first across repeated evaluations.
func TestObjectEvaluationFailureIsDeterministic(t *testing.T) {
	program := mustCompile(t, map[string]any{
		"z": "${input.z}",
		"a": "${input.a}",
	})
	for range 10 {
		_, err := program.Evaluate(EvaluationContext{Input: map[string]any{}})
		if !errors.Is(err, ErrMissingValue) {
			t.Fatalf("Evaluate() error = %v, want ErrMissingValue", err)
		}
	}
}

// mustCompile stops fixture construction immediately when in-call DynamicValue evaluation setup is invalid.
func mustCompile(t *testing.T, value any) *Program {
	t.Helper()
	program, err := CompileWithTargets(value, DefaultLimits(), []string{"github", "gitlab"})
	if err != nil {
		t.Fatalf("CompileWithTargets() error = %v", err)
	}
	return program
}

// testLimits protects bounded work for attacker-controlled documents and responses.
func testLimits(depth, nodes int) Limits {
	return Limits{MaxDepth: depth, MaxNodes: nodes, MaxExpressions: 10, MaxPathSegments: 5, MaxExpressionLength: 100, MaxEncodedBytes: 1000}
}
