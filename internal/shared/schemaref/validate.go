package schemaref

// validateNode follows schema vocabulary structurally, never refs or example/default application data.
func (index *Index) validateNode(node, root any, shared bool, depth int, budget *int) error {
	// A finite structural walk allows cyclic reference graphs without recursive graph expansion.
	if depth > MaxDepth || *budget <= 0 {
		return ErrLimit
	}
	*budget--
	object, ok := node.(map[string]any)
	// JSON Schema boolean roots have no references or child schemas to inspect.
	if !ok {
		return nil
	}
	// The only executable reference vocabulary supported by this contract is $ref.
	if reference, exists := object["$ref"]; exists {
		ref, valid := reference.(string)
		_, _, _, found := index.Resolve(root, ref)
		// Bad edges fail during admission, before a consumer can mistake them for unconstrained input.
		if !valid || !found {
			return ErrInvalid
		}
	}
	return index.validateChildren(object, root, shared, depth+1, budget)
}

// validateChildren distinguishes schema maps/arrays from opaque provider metadata.
func (index *Index) validateChildren(object map[string]any, root any, shared bool, depth int, budget *int) error {
	for key, value := range object {
		// Unknown keywords remain lossless inert data rather than acquiring reference semantics.
		switch SchemaKeywordKind(key) {
		case SchemaMap:
			// Named-map keys are identifiers, even when named examples or default.
			if err := index.validateMap(value, root, shared, depth, budget); err != nil {
				return err
			}
		case SchemaArray:
			// Composition arrays expose schemas, not the literal contents of enum/examples arrays.
			if err := index.validateArray(value, root, shared, depth, budget); err != nil {
				return err
			}
		case SchemaValue:
			// Each schema-bearing child retains the same owning document for local references.
			if err := index.validateNode(value, root, shared, depth, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateMap charges every named child against one shared structural-work budget.
func (index *Index) validateMap(value, root any, shared bool, depth int, budget *int) error {
	children, _ := value.(map[string]any)
	for _, child := range children {
		// A sibling cannot reset limits exhausted by a preceding branch.
		if err := index.validateNode(child, root, shared, depth, budget); err != nil {
			return err
		}
	}
	return nil
}

// validateArray preserves composition order while checking only schema-valued branches.
func (index *Index) validateArray(value, root any, shared bool, depth int, budget *int) error {
	children, _ := value.([]any)
	for _, child := range children {
		// Recursive descent is structurally bounded independently from reference cycles.
		if err := index.validateNode(child, root, shared, depth, budget); err != nil {
			return err
		}
	}
	return nil
}
