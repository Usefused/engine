package unified

import (
	"fmt"
	"strings"
	"unicode"
)

type referenceSource uint8

const (
	referenceInput referenceSource = iota + 1
	referenceResponse
	referenceTarget
)

type reference struct {
	source   referenceSource
	service  string
	path     []string
	optional bool
}

// compileExpression parses the bounded coalescing chain into references while
// preserving operand order and terminal optionality.
func compileExpression(expression string, limits Limits, allowedTargets []string) (node, error) {
	parts := strings.Split(expression, "??")
	if len(parts) == 0 {
		return nil, ErrInvalidExpression
	}
	references := make([]reference, 0, len(parts))
	for _, part := range parts {
		compiled, err := compileReference(strings.TrimSpace(part), limits, allowedTargets)
		if err != nil {
			return nil, err
		}
		references = append(references, compiled)
	}
	if len(references) == 1 {
		return expressionNode{references: references}, nil
	}
	return coalesceNode{references: references}, nil
}

// compileReference removes the optional marker and delegates namespace/path
// admission without accepting an empty operand.
func compileReference(expression string, limits Limits, allowedTargets []string) (reference, error) {
	if expression == "" {
		return reference{}, fmt.Errorf("%w: empty operand", ErrInvalidExpression)
	}
	optional := strings.HasSuffix(expression, "?")
	if optional {
		expression = strings.TrimSuffix(expression, "?")
	}
	return referenceFromExpression(expression, optional, limits, allowedTargets)
}

// referenceFromExpression splits a reviewed expression into source namespace, service target, and JSON path.
func referenceFromExpression(expression string, optional bool, limits Limits, allowedTargets []string) (reference, error) {
	if expression == "target" {
		if optional {
			return reference{}, fmt.Errorf("%w: target cannot have an optional marker", ErrInvalidExpression)
		}
		return reference{source: referenceTarget}, nil
	}
	if path, ok := strings.CutPrefix(expression, "input."); ok {
		segments, err := compilePath(path, limits.MaxPathSegments)
		return reference{source: referenceInput, path: segments, optional: optional}, err
	}
	if rest, ok := strings.CutPrefix(expression, "response."); ok {
		return compileResponseReference(rest, optional, limits.MaxPathSegments, allowedTargets)
	}
	return reference{}, fmt.Errorf("%w: unknown source", ErrInvalidExpression)
}

// compileResponseReference uses longest pre-sorted target matching so dotted
// service names cannot be mistaken for response path segments.
func compileResponseReference(rest string, optional bool, maxPathSegments int, allowedTargets []string) (reference, error) {
	for _, target := range allowedTargets {
		path, matched := strings.CutPrefix(rest, target+".")
		if !matched {
			continue
		}
		segments, err := compilePath(path, maxPathSegments)
		if err != nil {
			return reference{}, err
		}
		return reference{source: referenceResponse, service: target, path: segments, optional: optional}, nil
	}
	return reference{}, fmt.Errorf("%w: unknown response target", ErrInvalidExpression)
}

// compilePath splits a JSON traversal path and enforces the segment count and
// identifier grammar before it reaches evaluation.
func compilePath(value string, max int) ([]string, error) {
	parts := strings.Split(value, ".")
	if err := validateSegments(parts, max); err != nil {
		return nil, err
	}
	return parts, nil
}

// validateSegments rejects malformed segments before it can cross the bounded DynamicValue compilation boundary.
func validateSegments(parts []string, max int) error {
	if len(parts) > max {
		return fmt.Errorf("%w: maximum path segments exceeded", ErrLimitExceeded)
	}
	for _, part := range parts {
		if !validSegment(part) {
			return fmt.Errorf("%w: invalid path segment", ErrInvalidExpression)
		}
	}
	return nil
}

// validSegment rejects malformed segment before it can cross the bounded DynamicValue compilation boundary.
func validSegment(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
