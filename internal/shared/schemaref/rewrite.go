package schemaref

type SchemaChildKind uint8

const (
	OpaqueValue SchemaChildKind = iota
	SchemaValue
	SchemaMap
	SchemaArray
)

// SchemaKeywordKind keeps reference admission and relocation on the same schema
// vocabulary so opaque application values never acquire execution semantics.
func SchemaKeywordKind(keyword string) SchemaChildKind {
	// Only known schema-bearing keywords contain executable reference edges.
	switch keyword {
	case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas":
		return SchemaMap
	case "allOf", "anyOf", "oneOf", "prefixItems":
		return SchemaArray
	case "items", "additionalItems", "additionalProperties", "unevaluatedProperties", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "contentSchema":
		return SchemaValue
	default:
		return OpaqueValue
	}
}

// RewriteReferences relocates schema edges into caller-owned document scope
// without following them or modifying the source. Old resource IDs are removed
// so fragment references bind to that destination; opaque example data survives.
func RewriteReferences(root any, rewrite func(string) (string, error)) (any, error) {
	budget := MaxNodes
	return rewriteSchemaNode(root, rewrite, 0, &budget)
}

// rewriteSchemaNode copies only structural schema nodes within shared limits.
func rewriteSchemaNode(node any, rewrite func(string) (string, error), depth int, budget *int) (any, error) {
	// Relocation cannot bypass the same structural bound enforced at admission.
	if depth > MaxDepth || *budget <= 0 {
		return nil, ErrLimit
	}
	*budget--
	object, ok := node.(map[string]any)
	// Boolean schemas and inert scalar keywords require no recursive copying.
	if !ok {
		return node, nil
	}
	result := make(map[string]any, len(object))
	for key, value := range object {
		// Retaining an old base URI would redirect relocated fragment references away
		// from the destination document even though their component names are correct.
		if key == "$id" {
			continue
		}
		child, err := rewriteSchemaChild(key, value, rewrite, depth+1, budget)
		// An unrelocatable edge must never degrade to an unconstrained schema.
		if err != nil {
			return nil, err
		}
		result[key] = child
	}
	return result, nil
}

// rewriteSchemaChild handles the reference itself separately from child schema
// containers while preserving all unknown metadata by reference.
func rewriteSchemaChild(key string, value any, rewrite func(string) (string, error), depth int, budget *int) (any, error) {
	// Reference syntax is executable only at the root of a schema object.
	if key == "$ref" {
		ref, ok := value.(string)
		// Malformed references cannot become valid through a formatting transform.
		if !ok {
			return nil, ErrInvalid
		}
		return rewrite(ref)
	}
	// The shared classifier prevents drift between validation and export.
	switch SchemaKeywordKind(key) {
	case SchemaValue:
		return rewriteSchemaNode(value, rewrite, depth, budget)
	case SchemaMap:
		return rewriteSchemaMap(value, rewrite, depth, budget)
	case SchemaArray:
		return rewriteSchemaArray(value, rewrite, depth, budget)
	default:
		return value, nil
	}
}

// rewriteSchemaMap preserves literal property/definition names while rewriting
// only their schema-valued children.
func rewriteSchemaMap(value any, rewrite func(string) (string, error), depth int, budget *int) (any, error) {
	children, ok := value.(map[string]any)
	// Invalid non-schema shapes remain the authoritative validator's concern.
	if !ok {
		return value, nil
	}
	result := make(map[string]any, len(children))
	for name, child := range children {
		rewritten, err := rewriteSchemaNode(child, rewrite, depth, budget)
		// Every sibling consumes the same traversal budget.
		if err != nil {
			return nil, err
		}
		result[name] = rewritten
	}
	return result, nil
}

// rewriteSchemaArray preserves composition order without traversing opaque
// arrays such as enum and examples.
func rewriteSchemaArray(value any, rewrite func(string) (string, error), depth int, budget *int) (any, error) {
	children, ok := value.([]any)
	// The relocation layer never invents a schema array from another shape.
	if !ok {
		return value, nil
	}
	result := make([]any, len(children))
	for position, child := range children {
		rewritten, err := rewriteSchemaNode(child, rewrite, depth, budget)
		// A failed child stops export instead of producing a partial contract.
		if err != nil {
			return nil, err
		}
		result[position] = rewritten
	}
	return result, nil
}
