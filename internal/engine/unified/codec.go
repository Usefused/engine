package unified

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	wireLiteral       = "literal"
	wireObject        = "object"
	wireArray         = "array"
	wireReferenceNode = "reference"
	wireCoalesce      = "coalesce"
)

type wireProgram struct {
	SchemaVersion int      `json:"schema_version"`
	Root          wireNode `json:"root"`
}

type wireNode struct {
	Kind       string          `json:"kind"`
	Literal    json.RawMessage `json:"literal,omitempty"`
	Fields     []wireField     `json:"fields,omitempty"`
	Items      []wireNode      `json:"items,omitempty"`
	References []wireReference `json:"references,omitempty"`
}

type wireField struct {
	Key   string   `json:"key"`
	Value wireNode `json:"value"`
}

type wireReference struct {
	Source   string   `json:"source"`
	Service  string   `json:"service,omitempty"`
	Path     []string `json:"path,omitempty"`
	Optional bool     `json:"optional,omitempty"`
}

// EncodeProgram returns deterministic versioned JSON for Engine persistence.
// runtime loading must rehydrate executable data without retaining or
// reparsing the public expression text.
func EncodeProgram(program *Program, limits Limits) ([]byte, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if program == nil || program.root == nil {
		return nil, ErrInvalidProgram
	}
	state := codecState{limits: limits}
	root, err := state.encodeNode(program.root, 0)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(wireProgram{SchemaVersion: ProgramSchemaVersion, Root: root})
	if err != nil {
		return nil, fmt.Errorf("%w: encode", ErrInvalidProgram)
	}
	if len(encoded) > limits.MaxEncodedBytes {
		return nil, fmt.Errorf("%w: maximum encoded size exceeded", ErrLimitExceeded)
	}
	return encoded, nil
}

// DecodeProgram strictly validates and rehydrates a persisted program. Exact
// target validation prevents stored response references from gaining access to
// a binding absent from the immutable app runtime.
func DecodeProgram(encoded []byte, limits Limits, allowedTargets []string) (*Program, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > limits.MaxEncodedBytes {
		return nil, fmt.Errorf("%w: encoded size", ErrLimitExceeded)
	}
	targets, err := prepareAllowedTargets(allowedTargets)
	if err != nil {
		return nil, err
	}
	var persisted wireProgram
	if err := decodeStrictJSON(encoded, &persisted); err != nil {
		return nil, fmt.Errorf("%w: decode", ErrInvalidProgram)
	}
	if persisted.SchemaVersion != ProgramSchemaVersion {
		return nil, fmt.Errorf("%w: unsupported schema version", ErrInvalidProgram)
	}
	state := codecState{limits: limits, allowedTargets: targets}
	root, err := state.decodeNode(persisted.Root, 0)
	if err != nil {
		return nil, err
	}
	return &Program{root: root}, nil
}

type codecState struct {
	limits         Limits
	allowedTargets []string
	nodes          int
	expressions    int
}

// admitNode charges shared limits and rejects unsupported private DynamicValue bytecode shapes before allocation grows.
func (s *codecState) admitNode(depth int) error {
	if depth > s.limits.MaxDepth {
		return fmt.Errorf("%w: maximum depth exceeded", ErrLimitExceeded)
	}
	s.nodes++
	if s.nodes > s.limits.MaxNodes {
		return fmt.Errorf("%w: maximum node count exceeded", ErrLimitExceeded)
	}
	return nil
}

// encodeNode serializes node into canonical private bytes for stable hashing.
func (s *codecState) encodeNode(value node, depth int) (wireNode, error) {
	if err := s.admitNode(depth); err != nil {
		return wireNode{}, err
	}
	switch typed := value.(type) {
	case literalNode:
		return encodeLiteral(typed.value)
	case objectNode:
		return s.encodeObject(typed, depth)
	case arrayNode:
		return s.encodeArray(typed, depth)
	case expressionNode:
		return s.encodeReferences(wireReferenceNode, typed.references)
	case coalesceNode:
		return s.encodeReferences(wireCoalesce, typed.references)
	default:
		return wireNode{}, fmt.Errorf("%w: unknown node", ErrInvalidProgram)
	}
}

// encodeLiteral serializes literal into canonical private bytes for stable hashing.
func encodeLiteral(value any) (wireNode, error) {
	if !isJSONScalar(value) {
		return wireNode{}, fmt.Errorf("%w: unsupported literal", ErrInvalidProgram)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return wireNode{}, fmt.Errorf("%w: invalid literal", ErrInvalidProgram)
	}
	return wireNode{Kind: wireLiteral, Literal: encoded}, nil
}

// encodeObject serializes object into canonical private bytes for stable hashing.
func (s *codecState) encodeObject(value objectNode, depth int) (wireNode, error) {
	fields := append([]objectField(nil), value.fields...)
	sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
	encoded := make([]wireField, 0, len(fields))
	for index, field := range fields {
		if index > 0 && fields[index-1].key == field.key {
			return wireNode{}, fmt.Errorf("%w: duplicate object key", ErrInvalidProgram)
		}
		child, err := s.encodeNode(field.value, depth+1)
		if err != nil {
			return wireNode{}, err
		}
		encoded = append(encoded, wireField{Key: field.key, Value: child})
	}
	return wireNode{Kind: wireObject, Fields: encoded}, nil
}

// encodeArray serializes array into canonical private bytes for stable hashing.
func (s *codecState) encodeArray(value arrayNode, depth int) (wireNode, error) {
	items := make([]wireNode, 0, len(value.items))
	for _, item := range value.items {
		child, err := s.encodeNode(item, depth+1)
		if err != nil {
			return wireNode{}, err
		}
		items = append(items, child)
	}
	return wireNode{Kind: wireArray, Items: items}, nil
}

// encodeReferences serializes references into canonical private bytes for stable hashing.
func (s *codecState) encodeReferences(kind string, values []reference) (wireNode, error) {
	if (kind == wireReferenceNode && len(values) != 1) || (kind == wireCoalesce && len(values) < 2) {
		return wireNode{}, fmt.Errorf("%w: reference count", ErrInvalidProgram)
	}
	if err := s.admitExpression(); err != nil {
		return wireNode{}, err
	}
	if err := validateCompiledExpressionLength(values, s.limits); err != nil {
		return wireNode{}, err
	}
	references := make([]wireReference, 0, len(values))
	for _, value := range values {
		if err := validateCompiledReference(value, s.limits, nil); err != nil {
			return wireNode{}, err
		}
		references = append(references, encodeReference(value))
	}
	return wireNode{Kind: kind, References: references}, nil
}

// encodeReference serializes reference into canonical private bytes for stable hashing.
func encodeReference(value reference) wireReference {
	return wireReference{
		Source: referenceSourceName(value.source), Service: value.service,
		Path: append([]string(nil), value.path...), Optional: value.optional,
	}
}

// decodeNode restores node only after strict shape, limit, and namespace checks.
func (s *codecState) decodeNode(value wireNode, depth int) (node, error) {
	if err := s.admitNode(depth); err != nil {
		return nil, err
	}
	if err := validateWireNodeShape(value); err != nil {
		return nil, err
	}
	switch value.Kind {
	case wireLiteral:
		return decodeLiteral(value.Literal)
	case wireObject:
		return s.decodeObject(value.Fields, depth)
	case wireArray:
		return s.decodeArray(value.Items, depth)
	case wireReferenceNode:
		return s.decodeReferences(value.References, false)
	case wireCoalesce:
		return s.decodeReferences(value.References, true)
	default:
		return nil, fmt.Errorf("%w: unknown node kind", ErrInvalidProgram)
	}
}

// validateWireNodeShape rejects malformed wire node shape before it can cross the private DynamicValue bytecode boundary.
func validateWireNodeShape(value wireNode) error {
	switch value.Kind {
	case wireLiteral:
		return validateLiteralNodeShape(value)
	case wireObject:
		return validateObjectNodeShape(value)
	case wireArray:
		return validateArrayNodeShape(value)
	case wireReferenceNode, wireCoalesce:
		return validateReferenceNodeShape(value)
	default:
		return invalidNodeShape()
	}
}

// validateLiteralNodeShape rejects malformed literal node shape before it can cross the private DynamicValue bytecode boundary.
func validateLiteralNodeShape(value wireNode) error {
	if value.Literal == nil || value.Fields != nil || value.Items != nil || value.References != nil {
		return invalidNodeShape()
	}
	return nil
}

// validateObjectNodeShape rejects malformed object node shape before it can cross the private DynamicValue bytecode boundary.
func validateObjectNodeShape(value wireNode) error {
	if value.Literal != nil || value.Items != nil || value.References != nil {
		return invalidNodeShape()
	}
	return nil
}

// validateArrayNodeShape rejects malformed array node shape before it can cross the private DynamicValue bytecode boundary.
func validateArrayNodeShape(value wireNode) error {
	if value.Literal != nil || value.Fields != nil || value.References != nil {
		return invalidNodeShape()
	}
	return nil
}

// validateReferenceNodeShape rejects malformed reference node shape before it can cross the private DynamicValue bytecode boundary.
func validateReferenceNodeShape(value wireNode) error {
	if value.Literal != nil || value.Fields != nil || value.Items != nil {
		return invalidNodeShape()
	}
	return nil
}

// invalidNodeShape returns the shared corruption classification for malformed bytecode nodes.
func invalidNodeShape() error {
	return fmt.Errorf("%w: node has invalid fields", ErrInvalidProgram)
}

// decodeLiteral restores literal only after strict shape, limit, and namespace checks.
func decodeLiteral(encoded json.RawMessage) (node, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: literal decode", ErrInvalidProgram)
	}
	if _, err := decoder.Token(); err != io.EOF || !isJSONScalar(value) {
		return nil, fmt.Errorf("%w: literal shape", ErrInvalidProgram)
	}
	return literalNode{value: value}, nil
}

// decodeObject restores object only after strict shape, limit, and namespace checks.
func (s *codecState) decodeObject(values []wireField, depth int) (node, error) {
	fields := make([]objectField, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if _, exists := seen[value.Key]; exists {
			return nil, fmt.Errorf("%w: duplicate object key", ErrInvalidProgram)
		}
		seen[value.Key] = struct{}{}
		child, err := s.decodeNode(value.Value, depth+1)
		if err != nil {
			return nil, err
		}
		fields = append(fields, objectField{key: value.Key, value: child})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].key < fields[j].key })
	return objectNode{fields: fields}, nil
}

// decodeArray restores array only after strict shape, limit, and namespace checks.
func (s *codecState) decodeArray(values []wireNode, depth int) (node, error) {
	items := make([]node, 0, len(values))
	for _, value := range values {
		child, err := s.decodeNode(value, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, child)
	}
	return arrayNode{items: items}, nil
}

// decodeReferences restores references only after strict shape, limit, and namespace checks.
func (s *codecState) decodeReferences(values []wireReference, coalesce bool) (node, error) {
	if err := validateWireReferenceCount(len(values), coalesce); err != nil {
		return nil, err
	}
	if err := s.admitExpression(); err != nil {
		return nil, err
	}
	references := make([]reference, 0, len(values))
	for _, value := range values {
		decoded, err := decodeReference(value)
		if err != nil {
			return nil, err
		}
		if err := validateCompiledReference(decoded, s.limits, s.allowedTargets); err != nil {
			return nil, err
		}
		references = append(references, decoded)
	}
	if err := validateCompiledExpressionLength(references, s.limits); err != nil {
		return nil, err
	}
	if coalesce {
		return coalesceNode{references: references}, nil
	}
	return expressionNode{references: references}, nil
}

// validateWireReferenceCount rejects malformed wire reference count before it can cross the private DynamicValue bytecode boundary.
func validateWireReferenceCount(count int, coalesce bool) error {
	valid := count == 1
	if coalesce {
		valid = count >= 2
	}
	if !valid {
		return fmt.Errorf("%w: reference count", ErrInvalidProgram)
	}
	return nil
}

// admitExpression charges shared limits and rejects unsupported private DynamicValue bytecode shapes before allocation grows.
func (s *codecState) admitExpression() error {
	s.expressions++
	if s.expressions > s.limits.MaxExpressions {
		return fmt.Errorf("%w: maximum expression count exceeded", ErrLimitExceeded)
	}
	return nil
}

// decodeReference restores reference only after strict shape, limit, and namespace checks.
func decodeReference(value wireReference) (reference, error) {
	source := referenceSource(0)
	switch value.Source {
	case "input":
		source = referenceInput
	case "response":
		source = referenceResponse
	case "target":
		source = referenceTarget
	default:
		return reference{}, fmt.Errorf("%w: reference source", ErrInvalidProgram)
	}
	return reference{source: source, service: value.Service, path: append([]string(nil), value.Path...), optional: value.Optional}, nil
}

// validateCompiledReference rejects malformed compiled reference before it can cross the private DynamicValue bytecode boundary.
func validateCompiledReference(value reference, limits Limits, allowedTargets []string) error {
	if err := validateSegments(value.path, limits.MaxPathSegments); err != nil && value.source != referenceTarget {
		if errors.Is(err, ErrLimitExceeded) {
			return err
		}
		return fmt.Errorf("%w: reference path", ErrInvalidProgram)
	}
	return validateReferenceShape(value, allowedTargets)
}

// validateCompiledExpressionLength rejects malformed compiled expression length before it can cross the private DynamicValue bytecode boundary.
func validateCompiledExpressionLength(values []reference, limits Limits) error {
	length := 3
	for index, value := range values {
		if index > 0 {
			length += 2
		}
		length += compiledReferenceOperandLength(value)
	}
	if length > limits.MaxExpressionLength {
		return fmt.Errorf("%w: maximum expression length exceeded", ErrLimitExceeded)
	}
	return nil
}

// validateReferenceShape rejects malformed reference shape before it can cross the private DynamicValue bytecode boundary.
func validateReferenceShape(value reference, allowedTargets []string) error {
	switch value.source {
	case referenceInput:
		return validateInputReference(value)
	case referenceResponse:
		return validateResponseReference(value, allowedTargets)
	case referenceTarget:
		return validateTargetReference(value)
	default:
		return fmt.Errorf("%w: reference", ErrInvalidProgram)
	}

}

// validateInputReference rejects malformed input reference before it can cross the private DynamicValue bytecode boundary.
func validateInputReference(value reference) error {
	if value.service != "" || len(value.path) == 0 {
		return fmt.Errorf("%w: input reference", ErrInvalidProgram)
	}
	return nil
}

// validateResponseReference rejects malformed response reference before it can cross the private DynamicValue bytecode boundary.
func validateResponseReference(value reference, allowedTargets []string) error {
	invalidTarget := allowedTargets != nil && !containsTarget(allowedTargets, value.service)
	if len(value.path) == 0 || value.service == "" || invalidTarget {
		return fmt.Errorf("%w: response reference", ErrInvalidProgram)
	}
	return nil
}

// validateTargetReference rejects malformed target reference before it can cross the private DynamicValue bytecode boundary.
func validateTargetReference(value reference) error {
	if value.service != "" || len(value.path) != 0 || value.optional {
		return fmt.Errorf("%w: target reference", ErrInvalidProgram)
	}
	return nil
}

// containsTarget performs a bounded membership check used by private DynamicValue bytecode admission.
func containsTarget(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// compiledReferenceOperandLength measures the persisted source, service, path,
// and optional marker so decoded expressions cannot evade length limits.
func compiledReferenceOperandLength(value reference) int {
	length := len(referenceSourceName(value.source))
	if len(value.path) > 0 {
		length += len(strings.Join(value.path, ".")) + 1
	}
	if value.service != "" {
		length += len(value.service) + 1
	}
	if value.optional {
		length++
	}
	return length
}

// referenceSourceName maps internal reference kinds to their stable persisted wire labels.
func referenceSourceName(value referenceSource) string {
	switch value {
	case referenceInput:
		return "input"
	case referenceResponse:
		return "response"
	case referenceTarget:
		return "target"
	default:
		return ""
	}
}

// decodeStrictJSON restores strict json only after strict shape, limit, and namespace checks.
func decodeStrictJSON(encoded []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}
