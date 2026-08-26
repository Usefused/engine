package schemacontract

import (
	"reflect"

	"github.com/Usefused/engine/internal/shared/schemaref"
)

// Walk visits typed schema envelopes only, avoiding raw JSON, projections, and opaque example data.
func Walk[T any](value any, visit func(*T) error) error {
	budget := schemaref.MaxNodes
	// The wrapper shares traversal across wire and execution DTOs without converting their raw schemas.
	visitor := schemaVisitor{schemaType: reflect.TypeFor[*T](), visit: func(value reflect.Value) error { return visit(value.Interface().(*T)) }}
	return walkValue(reflect.ValueOf(value), visitor, 0, &budget)
}

type schemaVisitor struct {
	schemaType reflect.Type
	visit      func(reflect.Value) error
}

// walkValue bounds both recursive typed encodings and programmatically constructed cycles.
func walkValue(value reflect.Value, visit schemaVisitor, depth int, budget *int) error {
	// Optional top-level values have no schema envelopes to attach or validate.
	if !value.IsValid() {
		return nil
	}
	// Typed metadata must fit a finite walk even before JSON encoding rejects cycles.
	if depth > schemaref.MaxDepth || *budget <= 0 {
		return schemaref.ErrInvalid
	}
	*budget--
	// Schema pointers are terminal: their raw/projection content has separate authoritative validators.
	if value.Type() == visit.schemaType {
		// Omitted schema surfaces carry no reference semantics.
		if value.IsNil() {
			return nil
		}
		return visit.visit(value)
	}
	// Interfaces contain provider examples, not typed execution schema envelopes.
	switch value.Kind() {
	case reflect.Pointer:
		// Nil encoding pointers are ordinary absent metadata.
		if !value.IsNil() {
			return walkValue(value.Elem(), visit, depth+1, budget)
		}
	case reflect.Struct:
		return walkFields(value, visit, depth+1, budget)
	case reflect.Slice, reflect.Array, reflect.Map:
		return walkCollection(value, visit, depth+1, budget)
	}
	return nil
}

// walkFields ignores runtime-only attachments to prevent traversing cached dictionary state.
func walkFields(value reflect.Value, visit schemaVisitor, depth int, budget *int) error {
	for position := 0; position < value.NumField(); position++ {
		field := value.Type().Field(position)
		// Unexported and explicitly non-wire state is never an execution contract surface.
		if field.PkgPath != "" || field.Tag.Get("json") == "-" {
			continue
		}
		// Failures remain value-free and stop before any further allocations.
		if err := walkValue(value.Field(position), visit, depth, budget); err != nil {
			return err
		}
	}
	return nil
}

// walkCollection visits only containers of typed structs/pointers, excluding raw bytes and example maps.
func walkCollection(value reflect.Value, visit schemaVisitor, depth int, budget *int) error {
	kind := value.Type().Elem().Kind()
	// Primitive and interface elements cannot contain a statically declared schema envelope.
	if kind != reflect.Struct && kind != reflect.Pointer {
		return nil
	}
	// Map entries are copied values, but nested schema pointers retain their owning envelope.
	if value.Kind() == reflect.Map {
		iterator := value.MapRange()
		for iterator.Next() {
			// Every sibling consumes the same structural budget.
			if err := walkValue(iterator.Value(), visit, depth, budget); err != nil {
				return err
			}
		}
		return nil
	}
	for position := 0; position < value.Len(); position++ {
		// Slice and array entries use the same admission as map-owned contracts.
		if err := walkValue(value.Index(position), visit, depth, budget); err != nil {
			return err
		}
	}
	return nil
}
