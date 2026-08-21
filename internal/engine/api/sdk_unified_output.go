package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxUnifiedOutputDepth = 32

// compiledUnifiedOutputNode keeps the public validation shape paired with the
// private DynamicValue mapping produced from the same recursive authoring node.
type compiledUnifiedOutputNode struct {
	schema  any
	mapping any
}

// compileSDKUnifiedOutputDocument converts the single recursive authoring tree
// into the private validation schema and DynamicValue mapping used at runtime.
func compileSDKUnifiedOutputDocument(raw json.RawMessage, allowedTargets []string) (json.RawMessage, *unifiedOutputProgramSource, error) {
	value, err := decodeSDKUnifiedValue(raw)
	if err != nil {
		return nil, nil, err
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, nil, errors.New("root output must be a constructed object")
	}
	if fields["type"] != "object" {
		return nil, nil, errors.New("root output type must be object")
	}
	if _, exists := fields["value"]; exists {
		return nil, nil, errors.New("root output cannot declare value")
	}
	if _, exists := fields["properties"]; !exists {
		return nil, nil, errors.New("root output requires properties")
	}
	compiled, err := compileUnifiedExpandedOutput(fields, 0)
	if err != nil {
		return nil, nil, err
	}
	schema, err := json.Marshal(compiled.schema)
	if err != nil {
		return nil, nil, err
	}
	if err := validateUnifiedSchema("output", schema); err != nil {
		return nil, nil, err
	}
	mapping, err := json.Marshal(compiled.mapping)
	if err != nil {
		return nil, nil, err
	}
	return canonicalUnifiedJSON(schema), &unifiedOutputProgramSource{raw: canonicalUnifiedJSON(mapping), allowedTargets: allowedTargets}, nil
}

// unifiedOutputProgramSource defers DynamicValue compilation until callers
// have selected the exact response namespaces allowed by their output scope.
type unifiedOutputProgramSource struct {
	raw            json.RawMessage
	allowedTargets []string
}

// compileUnifiedExpandedOutput validates one explicitly typed output node.
func compileUnifiedExpandedOutput(fields map[string]any, depth int) (compiledUnifiedOutputNode, error) {
	if depth > maxUnifiedOutputDepth {
		return compiledUnifiedOutputNode{}, errors.New("output exceeds maximum depth")
	}
	if err := validateUnifiedOutputFieldNames(fields); err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	typeName, ok := fields["type"].(string)
	if !ok || !validSDKUnifiedOutputType(typeName) {
		return compiledUnifiedOutputNode{}, errors.New("output requires a valid type")
	}
	switch typeName {
	case "object":
		return compileUnifiedObjectOutput(fields, depth)
	case "array":
		return compileUnifiedArrayOutput(fields, depth)
	default:
		return compileUnifiedScalarOutput(fields, typeName)
	}
}

// validateUnifiedOutputFieldNames keeps misspelled schema or mapping controls inert.
func validateUnifiedOutputFieldNames(fields map[string]any) error {
	for name := range fields {
		switch name {
		case "type", "value", "properties", "required", "items", "additionalProperties":
		default:
			return fmt.Errorf("output contains unknown field %q", name)
		}
	}
	return nil
}

// compileUnifiedObjectOutput handles either a pass-through object or a recursively constructed object.
func compileUnifiedObjectOutput(fields map[string]any, depth int) (compiledUnifiedOutputNode, error) {
	value, hasValue := fields["value"]
	properties, hasProperties, err := unifiedOutputProperties(fields)
	if err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	if hasValue {
		return compileUnifiedPassThroughObject(fields, properties, hasProperties, value, depth)
	}
	if !hasProperties {
		return compiledUnifiedOutputNode{}, errors.New("constructed object output requires properties")
	}
	return compileUnifiedConstructedObject(fields, properties, depth)
}

// unifiedOutputProperties distinguishes a missing property map from a malformed declaration.
func unifiedOutputProperties(fields map[string]any) (map[string]any, bool, error) {
	value, exists := fields["properties"]
	if !exists {
		return nil, false, nil
	}
	properties, ok := value.(map[string]any)
	if !ok {
		return nil, false, errors.New("output properties must be an object")
	}
	return properties, true, nil
}

// compileUnifiedPassThroughObject keeps the expression as the mapping while its children describe validation only.
func compileUnifiedPassThroughObject(fields, properties map[string]any, hasProperties bool, value any, depth int) (compiledUnifiedOutputNode, error) {
	if _, exists := fields["items"]; exists {
		return compiledUnifiedOutputNode{}, errors.New("object output cannot declare items")
	}
	schema := map[string]any{"type": "object"}
	if hasProperties {
		compiled, err := compileUnifiedOutputSchemaProperties(properties, depth+1)
		if err != nil {
			return compiledUnifiedOutputNode{}, err
		}
		schema["properties"] = compiled
	}
	required, err := unifiedOutputRequired(fields, properties)
	if err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	if err := copyUnifiedOutputSchemaOptions(schema, fields, required); err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	return compiledUnifiedOutputNode{schema: schema, mapping: value}, nil
}

// compileUnifiedConstructedObject compiles property shorthand and validates required names.
func compileUnifiedConstructedObject(fields, properties map[string]any, depth int) (compiledUnifiedOutputNode, error) {
	if _, exists := fields["items"]; exists {
		return compiledUnifiedOutputNode{}, errors.New("object output cannot declare items")
	}
	schemaProperties := make(map[string]any, len(properties))
	mappingProperties := make(map[string]any, len(properties))
	for name, property := range properties {
		if name == "" {
			return compiledUnifiedOutputNode{}, errors.New("output property name cannot be empty")
		}
		compiled, err := compileUnifiedOutputProperty(property, depth+1)
		if err != nil {
			return compiledUnifiedOutputNode{}, fmt.Errorf("output property %q: %w", name, err)
		}
		schemaProperties[name], mappingProperties[name] = compiled.schema, compiled.mapping
	}
	required, err := unifiedOutputRequired(fields, properties)
	if err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	schema := map[string]any{"type": "object", "properties": schemaProperties}
	if err := copyUnifiedOutputSchemaOptions(schema, fields, required); err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	return compiledUnifiedOutputNode{schema: schema, mapping: mappingProperties}, nil
}

// compileUnifiedOutputProperty expands typed nodes and infers concise JSON shorthand.
func compileUnifiedOutputProperty(value any, depth int) (compiledUnifiedOutputNode, error) {
	if fields, ok := value.(map[string]any); ok {
		if _, expanded := fields["type"]; !expanded {
			return compiledUnifiedOutputNode{}, errors.New("object-valued output property requires type")
		}
		return compileUnifiedExpandedOutput(fields, depth)
	}
	if _, ok := value.([]any); ok {
		return compiledUnifiedOutputNode{}, errors.New("array-valued output property requires type")
	}
	return compiledUnifiedOutputNode{schema: inferredUnifiedOutputSchema(value), mapping: value}, nil
}

// compileUnifiedArrayOutput validates the pass-through array and its optional item schema.
func compileUnifiedArrayOutput(fields map[string]any, depth int) (compiledUnifiedOutputNode, error) {
	value, hasValue := fields["value"]
	if !hasValue {
		return compiledUnifiedOutputNode{}, errors.New("array output requires value")
	}
	if err := rejectUnifiedOutputFields(fields, "properties", "required", "additionalProperties"); err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	schema := map[string]any{"type": "array"}
	if items, exists := fields["items"]; exists {
		compiled, err := compileUnifiedOutputSchemaOnly(items, depth+1)
		if err != nil {
			return compiledUnifiedOutputNode{}, fmt.Errorf("output items: %w", err)
		}
		schema["items"] = compiled
	}
	return compiledUnifiedOutputNode{schema: schema, mapping: value}, nil
}

// compileUnifiedScalarOutput requires a value and rejects object/array-only controls.
func compileUnifiedScalarOutput(fields map[string]any, typeName string) (compiledUnifiedOutputNode, error) {
	value, ok := fields["value"]
	if !ok {
		return compiledUnifiedOutputNode{}, fmt.Errorf("%s output requires value", typeName)
	}
	if err := rejectUnifiedOutputFields(fields, "properties", "required", "items", "additionalProperties"); err != nil {
		return compiledUnifiedOutputNode{}, err
	}
	return compiledUnifiedOutputNode{schema: map[string]any{"type": typeName}, mapping: value}, nil
}

// compileUnifiedOutputSchemaOnly validates array item schemas without admitting mapping expressions.
func compileUnifiedOutputSchemaOnly(value any, depth int) (any, error) {
	if depth > maxUnifiedOutputDepth {
		return nil, errors.New("output exceeds maximum depth")
	}
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("item schema must be an object")
	}
	if _, exists := fields["value"]; exists {
		return nil, errors.New("output schema cannot declare value")
	}
	if err := validateUnifiedOutputFieldNames(fields); err != nil {
		return nil, err
	}
	typeName, ok := fields["type"].(string)
	if !ok || !validSDKUnifiedOutputType(typeName) {
		return nil, errors.New("output schema requires a valid type")
	}
	schema, err := compileUnifiedSchemaFields(fields, typeName, depth)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	if err := validateUnifiedSchema("output items", encoded); err != nil {
		return nil, err
	}
	return schema, nil
}

// compileUnifiedSchemaFields builds the schema-only half used by pass-through objects and arrays.
func compileUnifiedSchemaFields(fields map[string]any, typeName string, depth int) (map[string]any, error) {
	schema := map[string]any{"type": typeName}
	switch typeName {
	case "object":
		return compileUnifiedObjectSchema(fields, schema, depth)
	case "array":
		return compileUnifiedArraySchema(fields, schema, depth)
	default:
		if err := rejectUnifiedOutputFields(fields, "properties", "required", "items", "additionalProperties"); err != nil {
			return nil, err
		}
		return schema, nil
	}
}

// compileUnifiedObjectSchema recursively validates properties without compiling mappings.
func compileUnifiedObjectSchema(fields, schema map[string]any, depth int) (map[string]any, error) {
	if _, exists := fields["items"]; exists {
		return nil, errors.New("object output schema cannot declare items")
	}
	properties, present, err := unifiedOutputProperties(fields)
	if err != nil {
		return nil, err
	}
	if present {
		compiled, err := compileUnifiedOutputSchemaProperties(properties, depth+1)
		if err != nil {
			return nil, err
		}
		schema["properties"] = compiled
	}
	required, err := unifiedOutputRequired(fields, properties)
	if err != nil {
		return nil, err
	}
	if err := copyUnifiedOutputSchemaOptions(schema, fields, required); err != nil {
		return nil, err
	}
	return schema, nil
}

// compileUnifiedArraySchema recursively validates the optional item schema.
func compileUnifiedArraySchema(fields, schema map[string]any, depth int) (map[string]any, error) {
	if err := rejectUnifiedOutputFields(fields, "properties", "required", "additionalProperties"); err != nil {
		return nil, err
	}
	if items, exists := fields["items"]; exists {
		compiled, err := compileUnifiedOutputSchemaOnly(items, depth+1)
		if err != nil {
			return nil, err
		}
		schema["items"] = compiled
	}
	return schema, nil
}

// compileUnifiedOutputSchemaProperties validates every named child in a pass-through object definition.
func compileUnifiedOutputSchemaProperties(properties map[string]any, depth int) (map[string]any, error) {
	compiled := make(map[string]any, len(properties))
	for name, value := range properties {
		if name == "" {
			return nil, errors.New("output property name cannot be empty")
		}
		schema, err := compileUnifiedOutputSchemaOnly(value, depth)
		if err != nil {
			return nil, fmt.Errorf("output property %q: %w", name, err)
		}
		compiled[name] = schema
	}
	return compiled, nil
}

// unifiedOutputRequired restores and validates parent-owned required property names.
func unifiedOutputRequired(fields, properties map[string]any) ([]string, error) {
	value, exists := fields["required"]
	if !exists {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, errors.New("output required must be an array")
	}
	required := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, errors.New("output required entries must be non-empty strings")
		}
		if _, exists := properties[name]; !exists {
			return nil, fmt.Errorf("output required property %q is not declared", name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("output required property %q is duplicated", name)
		}
		seen[name], required = struct{}{}, append(required, name)
	}
	return required, nil
}

// copyUnifiedOutputSchemaOptions retains only validation fields implemented by the recursive DSL.
func copyUnifiedOutputSchemaOptions(schema, fields map[string]any, required []string) error {
	if len(required) > 0 {
		schema["required"] = required
	}
	if additional, exists := fields["additionalProperties"]; exists {
		value, ok := additional.(bool)
		if !ok {
			return errors.New("output additionalProperties must be boolean")
		}
		schema["additionalProperties"] = value
	}
	return nil
}

// inferredUnifiedOutputSchema derives shorthand type only from its JSON literal shape.
func inferredUnifiedOutputSchema(value any) any {
	switch typed := value.(type) {
	case string:
		return map[string]any{"type": "string"}
	case bool:
		return map[string]any{"type": "boolean"}
	case json.Number:
		if strings.ContainsAny(typed.String(), ".eE") {
			return map[string]any{"type": "number"}
		}
		return map[string]any{"type": "integer"}
	case nil:
		return map[string]any{"type": "null"}
	default:
		return map[string]any{}
	}
}

// validSDKUnifiedOutputType limits the DSL to JSON types implemented by both
// the Engine validator and generated TypeScript/Python schema compilers.
func validSDKUnifiedOutputType(value string) bool {
	switch value {
	case "string", "number", "integer", "boolean", "object", "array", "null":
		return true
	default:
		return false
	}
}

// rejectUnifiedOutputFields enforces the controls owned by each node type so
// schema-only metadata cannot accidentally become executable mapping syntax.
func rejectUnifiedOutputFields(fields map[string]any, names ...string) error {
	for _, name := range names {
		if _, exists := fields[name]; exists {
			return fmt.Errorf("output type cannot declare %s", name)
		}
	}
	return nil
}
