package unified

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CompileWithTargets converts a JSON/YAML-like value into an immutable program
// and admits responses only for the exact binding keys supplied by the caller.
// An empty target set permits only literal, input, and target mappings.
func CompileWithTargets(value any, limits Limits, allowedTargets []string) (*Program, error) {
	return compileWithTargets(value, limits, allowedTargets)
}

// compileWithTargets validates shared budgets and response namespaces before
// recursively building the immutable mapping AST.
func compileWithTargets(value any, limits Limits, allowedTargets []string) (*Program, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	targets, err := prepareAllowedTargets(allowedTargets)
	if err != nil {
		return nil, err
	}
	if err := validateEncodedValueSize(value, limits.MaxEncodedBytes); err != nil {
		return nil, err
	}
	compiler := compiler{limits: limits, allowedTargets: targets}
	// depth is measured as edges from the root, matching config parsing;
	// the root itself still counts toward the independent node budget.
	root, err := compiler.compile(value, 0)
	if err != nil {
		return nil, err
	}
	return &Program{root: root}, nil
}

type compiler struct {
	limits         Limits
	allowedTargets []string
	nodes          int
	expressions    int
}

// validateLimits rejects malformed limits before it can cross the bounded DynamicValue compilation boundary.
func validateLimits(limits Limits) error {
	if limits.MaxDepth <= 0 || limits.MaxNodes <= 0 || limits.MaxExpressions <= 0 || limits.MaxPathSegments <= 0 || limits.MaxExpressionLength <= 0 || limits.MaxEncodedBytes <= 0 {
		return fmt.Errorf("%w: limits must be positive", ErrLimitExceeded)
	}
	return nil
}

// validateEncodedValueSize rejects malformed encoded value size before it can cross the bounded DynamicValue compilation boundary.
func validateEncodedValueSize(value any, max int) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%w: value is not JSON-compatible", ErrInvalidValue)
	}
	if len(encoded) > max {
		return fmt.Errorf("%w: maximum encoded size exceeded", ErrLimitExceeded)
	}
	return nil
}

// compile charges the shared node/depth budget before dispatching each JSON
// value to its scalar, object, array, or expression compiler.
func (c *compiler) compile(value any, depth int) (node, error) {
	if err := c.admitNode(depth); err != nil {
		return nil, err
	}
	switch typed := value.(type) {
	case map[string]any:
		return c.compileObject(typed, depth)
	case []any:
		return c.compileArray(typed, depth)
	case string:
		return c.compileString(typed)
	default:
		if isJSONScalar(typed) {
			return compileLiteral(typed)
		}
		return nil, fmt.Errorf("%w: unsupported scalar type", ErrInvalidValue)
	}
}

// compileLiteral canonicalizes JSON numbers while preserving nil and booleans,
// keeping plan-time and decoded-program evaluation identical.
func compileLiteral(value any) (node, error) {
	switch value.(type) {
	case nil, bool:
		return literalNode{value: value}, nil
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid numeric literal", ErrInvalidValue)
		}
		// JSON has one number type. Canonicalizing it here ensures plan-time
		// evaluation and evaluation after persisted-program decode are identical.
		return literalNode{value: json.Number(encoded)}, nil
	}
}

// admitNode charges shared limits and rejects unsupported bounded DynamicValue compilation shapes before allocation grows.
func (c *compiler) admitNode(depth int) error {
	if depth > c.limits.MaxDepth {
		return fmt.Errorf("%w: maximum depth exceeded", ErrLimitExceeded)
	}
	c.nodes++
	if c.nodes > c.limits.MaxNodes {
		return fmt.Errorf("%w: maximum node count exceeded", ErrLimitExceeded)
	}
	return nil
}

// compileObject sorts keys before compiling fields so equivalent maps produce
// identical persisted programs and hashes.
func (c *compiler) compileObject(value map[string]any, depth int) (node, error) {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]objectField, 0, len(keys))
	for _, key := range keys {
		child, err := c.compile(value[key], depth+1)
		if err != nil {
			return nil, err
		}
		fields = append(fields, objectField{key: key, value: child})
	}
	return objectNode{fields: fields}, nil
}

// compileArray preserves element order while charging every child against the
// same depth and node limits.
func (c *compiler) compileArray(value []any, depth int) (node, error) {
	items := make([]node, 0, len(value))
	for _, item := range value {
		child, err := c.compile(item, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, child)
	}
	return arrayNode{items: items}, nil
}

// compileString treats only complete DynamicValue expressions as executable;
// all other strings remain literal JSON data.
func (c *compiler) compileString(value string) (node, error) {
	if !strings.Contains(value, "${") {
		return literalNode{value: value}, nil
	}
	if len(value) > c.limits.MaxExpressionLength {
		return nil, fmt.Errorf("%w: maximum expression length exceeded", ErrLimitExceeded)
	}
	expression, ok := completeExpression(value)
	if !ok {
		return nil, fmt.Errorf("%w: expressions must occupy the complete scalar", ErrInvalidExpression)
	}
	c.expressions++
	if c.expressions > c.limits.MaxExpressions {
		return nil, fmt.Errorf("%w: maximum expression count exceeded", ErrLimitExceeded)
	}
	return compileExpression(expression, c.limits, c.allowedTargets)
}

// completeExpression requires an entire string to be one DynamicValue expression rather than interpolation.
func completeExpression(value string) (string, bool) {
	if len(value) < 4 || !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	expression := value[2 : len(value)-1]
	if expression != strings.TrimSpace(expression) || strings.Contains(expression, "${") || strings.Contains(expression, "}") {
		return "", false
	}
	return expression, true
}

// isJSONScalar limits literal compilation to values with an unambiguous JSON representation.
func isJSONScalar(value any) bool {
	switch value.(type) {
	case nil, bool, string, json.Number,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return true
	default:
		return false
	}
}

// prepareAllowedTargets normalizes response namespaces before compiling any expression.
func prepareAllowedTargets(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	targets := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			return nil, fmt.Errorf("%w: response target must not be empty", ErrInvalidValue)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("%w: response targets must be unique", ErrInvalidValue)
		}
		seen[value] = struct{}{}
		targets = append(targets, value)
	}
	// configured keys are opaque and may contain dots. Longest-first
	// matching makes `response.acme.crm.id` resolve `acme.crm` rather than
	// incorrectly accepting an also-configured `acme` prefix.
	sort.Slice(targets, func(i, j int) bool {
		if len(targets[i]) == len(targets[j]) {
			return targets[i] < targets[j]
		}
		return len(targets[i]) > len(targets[j])
	})
	return targets, nil
}
