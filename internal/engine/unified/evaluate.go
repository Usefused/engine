package unified

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type literalNode struct {
	value any
}

// evaluate returns the literal captured when the mapping was compiled.
func (n literalNode) evaluate(EvaluationContext) (any, error) {
	return n.value, nil
}

type objectField struct {
	key   string
	value node
}

type objectNode struct {
	fields []objectField
}

// evaluate builds an object from the fields that remain present at runtime.
func (n objectNode) evaluate(ctx EvaluationContext) (any, error) {
	result := make(map[string]any, len(n.fields))
	for _, field := range n.fields {
		value, err := field.value.evaluate(ctx)
		if err != nil {
			return nil, err
		}
		if !IsOmitted(value) {
			result[field.key] = value
		}
	}
	return result, nil
}

type arrayNode struct {
	items []node
}

// evaluate builds an array while removing optional values that were not found.
func (n arrayNode) evaluate(ctx EvaluationContext) (any, error) {
	result := make([]any, 0, len(n.items))
	for _, item := range n.items {
		value, err := item.evaluate(ctx)
		if err != nil {
			return nil, err
		}
		// compacting omitted optional elements keeps the result valid JSON;
		// leaking an internal sentinel or inventing null would change its meaning.
		if !IsOmitted(value) {
			result = append(result, value)
		}
	}
	return result, nil
}

type expressionNode struct {
	references []reference
}

// evaluate resolves a required or optional reference from the admitted context.
func (n expressionNode) evaluate(ctx EvaluationContext) (any, error) {
	ref := n.references[0]
	value, found := evaluateReference(ref, ctx)
	if found {
		return value, nil
	}
	if ref.optional {
		return omitted, nil
	}
	return nil, ErrMissingValue
}

type coalesceNode struct {
	references []reference
}

type templateNode struct {
	parts    []node
	maxBytes int
}

// evaluate renders a mixed string while preserving expression-only mappings
// as typed values in their existing expression nodes.
func (n templateNode) evaluate(ctx EvaluationContext) (any, error) {
	var result strings.Builder
	for _, part := range n.parts {
		value, err := part.evaluate(ctx)
		if err != nil {
			return nil, err
		}
		if IsOmitted(value) {
			continue
		}
		text, err := interpolationScalar(value)
		if err != nil {
			return nil, err
		}
		if len(text) > n.maxBytes-result.Len() {
			return nil, fmt.Errorf("%w: maximum interpolated size exceeded", ErrLimitExceeded)
		}
		result.WriteString(text)
	}
	return result.String(), nil
}

// interpolationScalar formats only values with one deterministic JSON scalar
// spelling. Objects, arrays, and null must be mapped as complete expressions.
func interpolationScalar(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case json.Number:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return "", fmt.Errorf("%w: interpolation requires a finite JSON scalar", ErrInvalidValue)
		}
		return string(encoded), nil
	default:
		if !isJSONNumeric(value) {
			return "", fmt.Errorf("%w: interpolation requires a non-null scalar", ErrInvalidValue)
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", fmt.Errorf("%w: interpolation requires a finite JSON scalar", ErrInvalidValue)
		}
		return string(encoded), nil
	}
}

// isJSONNumeric admits runtime numeric forms without widening interpolation to
// nil, objects, arrays, or arbitrary Stringer implementations.
func isJSONNumeric(value any) bool {
	switch value.(type) {
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}

// evaluate returns the first non-null reference in a compiled fallback chain.
func (n coalesceNode) evaluate(ctx EvaluationContext) (any, error) {
	for _, ref := range n.references {
		value, found := evaluateReference(ref, ctx)
		if found && value != nil {
			return value, nil
		}
	}
	// optionality belongs to the terminal fallback. An optional earlier
	// operand permits fallthrough but cannot weaken a required final operand.
	if n.references[len(n.references)-1].optional {
		return omitted, nil
	}
	return nil, ErrMissingValue
}

// evaluateReference resolves one compiled reference without widening its response scope.
func evaluateReference(ref reference, ctx EvaluationContext) (any, bool) {
	switch ref.source {
	case referenceTarget:
		return ctx.Target, true
	case referenceInput:
		return lookupPath(ctx.Input, ref.path)
	case referenceResponse:
		// dependency evaluation supplies only the response namespaces admitted
		// by compilation, so runtime lookup cannot widen graph dataflow authority.
		if ctx.Responses != nil {
			response, ok := ctx.Responses[ref.service]
			if !ok {
				return nil, false
			}
			return lookupPath(response, ref.path)
		}
		// output projection remains current-target-only even when the caller
		// is executing several targets in one logical operation.
		if ref.service != ctx.Target {
			return nil, false
		}
		return lookupPath(ctx.Response, ref.path)
	default:
		return nil, false
	}
}

// lookupPath reads a bounded JSON path without copying unrelated response data.
func lookupPath(value any, path []string) (any, bool) {
	current := value
	for _, segment := range path {
		next, found := lookupSegment(current, segment)
		if !found {
			return nil, false
		}
		current = next
	}
	return current, true
}

// lookupSegment selects one field or array position from a JSON-compatible value.
func lookupSegment(value any, segment string) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		item, ok := typed[segment]
		return item, ok
	case []any:
		return lookupArraySegment(typed, segment)
	default:
		return nil, false
	}
}

// lookupArraySegment parses and bounds-checks one array path component.
func lookupArraySegment(value []any, segment string) (any, bool) {
	index, err := strconv.Atoi(segment)
	if err != nil || index < 0 || index >= len(value) {
		return nil, false
	}
	return value[index], true
}

// String redacts compiled mapping internals so logs cannot expose private provider shapes.
func (p *Program) String() string {
	// compiled mappings may describe private provider shapes, so the
	// program deliberately has no detailed debug serialization.
	if p == nil || p.root == nil {
		return "dynamic-value<invalid>"
	}
	return fmt.Sprintf("dynamic-value<%T>", p.root)
}
