package sandbox

import (
	"fmt"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/getkin/kin-openapi/openapi3"
)

// validateCallParams is the server-side backstop against hallucinated calls
// (design doc, "Guarding Against Hallucinated Calls"): search_docs hands the
// model this exact schema before it may reference an operationId, but
// nothing stops a script from calling with the wrong shape anyway. This
// re-checks params against the same fixture-sourced schema search_docs
// returned, before engineExecuteCore ever resolves a real endpoint or
// touches a vendor. A rejection here is a clean, script-visible error --
// never a malformed request reaching the vendor.
func validateCallParams(op *FixtureOperation, params map[string]any) error {
	if err := validateRequiredParameters(op.Parameters, params); err != nil {
		return err
	}
	if op.RequestBody != nil {
		if err := validateRequestBody(op, params); err != nil {
			return err
		}
	}
	return nil
}

// validateRequiredParameters checks that every required path/query/header
// parameter declared on the operation is present in params.
func validateRequiredParameters(declared []models.Parameter, params map[string]any) error {
	for _, p := range declared {
		if !p.Required {
			continue
		}
		if _, ok := params[p.Name]; !ok {
			return fmt.Errorf("missing required parameter %q (in %s)", p.Name, p.In)
		}
	}
	return nil
}

// validateRequestBody builds the same "everything not a declared
// path/query/header parameter" body object the dispatcher itself constructs
// (dispatcher.go: determineParamLocation/applyParam default case), then
// validates it against the operation's RequestBody schema. Mirroring that
// convention here -- rather than expecting a nested {path,query,...,body}
// shape -- means validation checks the request the same way it will
// eventually be routed, with one source of truth for what "the body" means
// for a flat params map.
func validateRequestBody(op *FixtureOperation, params map[string]any) error {
	declaredNames := make(map[string]struct{}, len(op.Parameters))
	for _, p := range op.Parameters {
		declaredNames[p.Name] = struct{}{}
	}

	bodyParams := make(map[string]any)
	for k, v := range params {
		if _, isDeclaredParam := declaredNames[k]; !isDeclaredParam {
			bodyParams[k] = v
		}
	}

	schema, err := modelSchemaToOpenAPI(op.RequestBody)
	if err != nil {
		return fmt.Errorf("invalid operation schema for %q: %w", op.OperationID, err)
	}

	if err := schema.VisitJSON(bodyParams); err != nil {
		return fmt.Errorf("request body for %q failed schema validation: %w", op.OperationID, err)
	}
	return nil
}

// modelSchemaToOpenAPI converts the fixture's models.Schema into the
// openapi3.Schema shape kin-openapi's validator needs. A narrow, one-way
// converter scoped to exactly what call() validation requires (type,
// properties, items, required) -- not a general bidirectional mapper. The
// Registry spec ingestion owns the opposite direction (openapi3 ->
// models.Schema), which is a distinct concern from validating an
// already-materialized fixture schema here.
func modelSchemaToOpenAPI(s *models.Schema) (*openapi3.Schema, error) {
	if s == nil {
		return openapi3.NewObjectSchema(), nil
	}

	schema := &openapi3.Schema{
		Type:     schemaTypeOrDefault(s.Type),
		Format:   s.Format,
		Required: s.Required,
	}

	if len(s.Properties) > 0 {
		schema.Properties = make(openapi3.Schemas, len(s.Properties))
		for name, propSchema := range s.Properties {
			propSchema := propSchema
			converted, err := modelSchemaToOpenAPI(&propSchema)
			if err != nil {
				return nil, err
			}
			schema.Properties[name] = openapi3.NewSchemaRef("", converted)
		}
	}

	if s.Items != nil {
		itemSchema, err := modelSchemaToOpenAPI(s.Items)
		if err != nil {
			return nil, err
		}
		schema.Items = openapi3.NewSchemaRef("", itemSchema)
	}

	return schema, nil
}

// schemaTypeOrDefault treats an unspecified type as "object": the fixture's
// RequestBody schemas describe JSON request bodies, and an empty Type field
// (common for hand-authored fixtures, e.g. fixture.json's request bodies)
// should validate as a permissive object rather than failing kin-openapi's
// type check outright.
func schemaTypeOrDefault(t string) *openapi3.Types {
	if t == "" {
		t = "object"
	}
	return &openapi3.Types{t}
}
