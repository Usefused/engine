// Package unified compiles and evaluates the bounded DynamicValue mappings
// used by SDK Unified Operations.
package unified

import "errors"

const (
	DefaultMaxDepth            = 32
	DefaultMaxNodes            = 10_000
	DefaultMaxExpressions      = 512
	DefaultMaxPathSegments     = 32
	DefaultMaxExpressionLength = 4 << 10
	DefaultMaxEncodedBytes     = 1 << 20

	// ProgramSchemaVersion is the newest private DynamicValue bytecode schema.
	// The base schema remains canonical for trees that do not need a template
	// node; version 2 is selected only when interpolation is actually present.
	ProgramSchemaVersion     = 2
	baseProgramSchemaVersion = 1
)

var (
	ErrInvalidValue       = errors.New("invalid dynamic value")
	ErrInvalidExpression  = errors.New("invalid dynamic value expression")
	ErrLimitExceeded      = errors.New("dynamic value limit exceeded")
	ErrMissingValue       = errors.New("dynamic value source is missing")
	ErrInvalidProgram     = errors.New("invalid compiled dynamic value program")
	ErrInvalidDefinitions = errors.New("invalid unified operation definitions")
)

// Limits bounds mapping compilation. Runtime request and response documents
// retain their own transport and schema limits.
type Limits struct {
	MaxDepth            int
	MaxNodes            int
	MaxExpressions      int
	MaxPathSegments     int
	MaxExpressionLength int
	MaxEncodedBytes     int
}

// DefaultLimits returns the shared byte, node, depth, and reference budgets used by compile and decode admission.
func DefaultLimits() Limits {
	return Limits{
		MaxDepth:            DefaultMaxDepth,
		MaxNodes:            DefaultMaxNodes,
		MaxExpressions:      DefaultMaxExpressions,
		MaxPathSegments:     DefaultMaxPathSegments,
		MaxExpressionLength: DefaultMaxExpressionLength,
		MaxEncodedBytes:     DefaultMaxEncodedBytes,
	}
}

// EvaluationContext exposes either one current response for output projection
// or an explicitly bounded response map for dependency and rollback mappings.
type EvaluationContext struct {
	Input     any
	Target    string
	Response  any
	Responses map[string]any
}

// Program is an immutable compiled DynamicValue tree and is safe for
// concurrent evaluation.
type Program struct {
	root node
}

// Evaluate interprets one admitted mapping node using only caller and response namespaces in scope.
func (p *Program) Evaluate(ctx EvaluationContext) (any, error) {
	if p == nil || p.root == nil {
		return nil, ErrInvalidValue
	}
	return p.root.evaluate(ctx)
}

type omittedValue struct{}

var omitted = omittedValue{}

// IsOmitted reports whether a root optional expression had no value. Within
// objects and arrays the evaluator consumes this sentinel by omitting the
// corresponding member.
func IsOmitted(value any) bool {
	_, ok := value.(omittedValue)
	return ok
}

type node interface {
	evaluate(EvaluationContext) (any, error)
}
