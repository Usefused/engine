package sandbox

import (
	"reflect"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// mcpSourceSchemaAdmission discovers schema contracts in the immutable source
// shape while sharing all schema parsing and budgets with fixture admission.
type mcpSourceSchemaAdmission struct {
	budget  mcpSchemaAdmission
	pending []reflect.Value
}

// validateMCPSourceEndpointSchemas bounds source schemas before the endpoint's
// existing JSON conversion can recurse into or allocate their full projection.
func validateMCPSourceEndpointSchemas(endpoint fusedobject.Endpoint) error {
	admission := &mcpSourceSchemaAdmission{}
	// The root endpoint owns one traversal reservation like every queued child.
	if err := admission.enqueue(reflect.ValueOf(endpoint)); err != nil {
		return err
	}
	for len(admission.pending) > 0 {
		index := len(admission.pending) - 1
		value := admission.pending[index]
		admission.pending[index] = reflect.Value{}
		admission.pending = admission.pending[:index]
		// Every pending value was reserved before its queue slot was allocated.
		if err := admission.admitValue(value); err != nil {
			return err
		}
	}
	return nil
}

// enqueue admits only source contract types that can contain an MCP-visible
// schema; unrelated endpoint documentation and routing fields stay out of scope.
func (a *mcpSourceSchemaAdmission) enqueue(value reflect.Value) error {
	// Type filtering prevents arbitrary Example objects from becoming a second discovery path.
	if !mcpSourceSchemaType(value.Type()) {
		return nil
	}
	if err := a.reserve(1); err != nil {
		return err
	}
	a.pending = append(a.pending, value)
	return nil
}

// reserve charges pending source traversal immediately so branching cycles
// cannot grow the worklist faster than the fixture-wide aggregate ceiling.
func (a *mcpSourceSchemaAdmission) reserve(count int) error {
	// Pending and completed source work share the existing aggregate node budget.
	if count > maxMCPAggregateSchemaNodes-a.budget.aggregateNodes {
		return ErrMCPSchemaAggregateNodesLimit
	}
	a.budget.aggregateNodes += count
	return nil
}

// admitValue intercepts canonical schema contracts before walking their known
// containers, reusing the exact raw/projection admission used by model fixtures.
func (a *mcpSourceSchemaAdmission) admitValue(value reflect.Value) error {
	value, err := mcpProjectionValue(value)
	// Source contracts contain ordinary pointers, but malformed pointer chains still fail closed.
	if err != nil {
		return err
	}
	// Optional nil schema/content pointers contribute no further traversal work.
	if !value.IsValid() {
		return nil
	}
	if contract, ok := value.Interface().(fusedobject.SchemaContract); ok {
		return a.budget.admitSchemaParts(contract.Raw, contract.Projection)
	}
	// Only the known contract/container shapes can have reached this queue.
	switch value.Kind() {
	case reflect.Struct:
		return a.admitStruct(value)
	case reflect.Map:
		return a.admitMap(value)
	case reflect.Slice, reflect.Array:
		return a.admitSlice(value)
	default:
		return ErrMCPSchemaInvalid
	}
}

// admitStruct follows the source model's own field types rather than copying
// every parameter/header/media schema path into another hand-written visitor.
func (a *mcpSourceSchemaAdmission) admitStruct(value reflect.Value) error {
	for index := 0; index < value.NumField(); index++ {
		// enqueue filters unrelated primitive and metadata fields before charging work.
		if err := a.enqueue(value.Field(index)); err != nil {
			return err
		}
	}
	return nil
}

// admitMap reserves complete map fanout before allocating queued values,
// including recursive RequestEncoding maps from programmatic fixtures.
func (a *mcpSourceSchemaAdmission) admitMap(value reflect.Value) error {
	if err := a.reserve(value.Len()); err != nil {
		return err
	}
	iterator := value.MapRange()
	// MapRange avoids an unbounded intermediate key slice before the reservation check.
	for iterator.Next() {
		a.pending = append(a.pending, iterator.Value())
	}
	return nil
}

// admitSlice reserves complete prefix/representation width before appending
// any children, preserving the same pending-work invariant as source maps.
func (a *mcpSourceSchemaAdmission) admitSlice(value reflect.Value) error {
	if err := a.reserve(value.Len()); err != nil {
		return err
	}
	for index := 0; index < value.Len(); index++ {
		// Child types are known because only relevant container types enter the queue.
		a.pending = append(a.pending, value.Index(index))
	}
	return nil
}

// mcpSourceSchemaType recognizes containers of owned schema-bearing contracts
// while skipping unrelated endpoint fields without inspecting their contents.
func mcpSourceSchemaType(valueType reflect.Type) bool {
	// Existing source containers unwrap to a known struct after at most a few hops.
	for hops := 0; hops <= maxMCPSchemaDepth; hops++ {
		switch valueType.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			valueType = valueType.Elem()
		default:
			return mcpSourceSchemaStruct(valueType)
		}
	}
	return false
}

// mcpSourceSchemaStruct limits discovery to the canonical physical contract
// types; projection recursion itself remains owned by the shared schema walker.
func mcpSourceSchemaStruct(valueType reflect.Type) bool {
	// Grouping these owned types avoids accepting arbitrary Example structs with custom behavior.
	switch valueType {
	case reflect.TypeFor[fusedobject.Endpoint](), reflect.TypeFor[fusedobject.Parameter](),
		reflect.TypeFor[fusedobject.ParameterContent](), reflect.TypeFor[fusedobject.RequestContent](),
		reflect.TypeFor[fusedobject.RequestRepresentation](), reflect.TypeFor[fusedobject.RequestEncoding](),
		reflect.TypeFor[fusedobject.HeaderContract](), reflect.TypeFor[fusedobject.ResponseRepresentation](),
		reflect.TypeFor[fusedobject.ResponseContract](), reflect.TypeFor[fusedobject.SchemaContract]():
		return true
	default:
		return false
	}
}
