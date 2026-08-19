package unified

import (
	"fmt"
	"strconv"
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
