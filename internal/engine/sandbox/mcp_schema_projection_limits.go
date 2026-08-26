package sandbox

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// mcpProjectionWork keeps an in-memory JSON value and its container depth on
// an explicit stack instead of delegating unbounded recursion to json.Marshal.
type mcpProjectionWork struct {
	value reflect.Value
	depth int
}

// mcpProjectionMeasure reserves nodes as work is queued and charges exact
// encoded sizes before the complete projection is ever serialized.
type mcpProjectionMeasure struct {
	pending      []mcpProjectionWork
	nodes        int
	encodedBytes int
}

// mcpProjectionMember preserves the model's own JSON field names so admission
// does not duplicate a hand-maintained projection schema.
type mcpProjectionMember struct {
	name  string
	value reflect.Value
}

// measureMCPProjection bounds the model projection and JSON-native examples
// before whole-document encoding can recurse or allocate over-budget output.
func measureMCPProjection(projection any) (int, error) {
	measure := &mcpProjectionMeasure{
		pending: []mcpProjectionWork{{value: reflect.ValueOf(projection), depth: 1}}, nodes: 1,
	}
	// Each queued value already owns a node reservation, keeping stack growth bounded.
	for len(measure.pending) > 0 {
		index := len(measure.pending) - 1
		work := measure.pending[index]
		measure.pending[index] = mcpProjectionWork{}
		measure.pending = measure.pending[:index]
		if err := measure.admitValue(work); err != nil {
			return 0, err
		}
	}
	return measure.nodes, nil
}

// admitValue dispatches only JSON-native shapes or the known projection model;
// custom encoders cannot run arbitrary work behind the admission boundary.
func (m *mcpProjectionMeasure) admitValue(work mcpProjectionWork) error {
	value, err := mcpProjectionValue(work.value)
	// Invalid pointer chains are rejected before any serializer can follow them.
	if err != nil {
		return err
	}
	// Nil interfaces and pointers encode as one JSON null value.
	if !value.IsValid() {
		return m.addBytes(4)
	}
	// RawMessage is the sole admitted custom encoder because its implementation is standard and bounded here.
	if raw, ok := value.Interface().(json.RawMessage); ok {
		return m.admitRawExample(raw, work.depth)
	}
	// Even scalar aliases may implement arbitrary encoders, so reject those before dispatch.
	if mcpProjectionHasCustomEncoder(value.Type()) {
		return ErrMCPSchemaInvalid
	}
	// Container-specific helpers reserve children before appending them to the stack.
	switch value.Kind() {
	case reflect.Struct:
		return m.admitStruct(value, work.depth)
	case reflect.Map:
		return m.admitMap(value, work.depth)
	case reflect.Slice, reflect.Array:
		return m.admitArray(value, work.depth)
	default:
		return m.admitScalar(value)
	}
}

// mcpProjectionValue unwraps ordinary interfaces/pointers with a finite hop
// budget, including pointer-only cycles that contain no JSON container nodes.
func mcpProjectionValue(value reflect.Value) (reflect.Value, error) {
	// The hop ceiling is independent from container counting because pointer chains add no JSON nodes.
	for hops := 0; hops <= maxMCPSchemaDepth; hops++ {
		if !value.IsValid() {
			return value, nil
		}
		// Concrete JSON values are inspected by the shape-specific admission helper.
		if value.Kind() != reflect.Interface && value.Kind() != reflect.Pointer {
			return value, nil
		}
		// Nil pointers are JSON null and never invoke custom marshaler methods.
		if value.IsNil() {
			return reflect.Value{}, nil
		}
		// Method-bearing pointers are not JSON-native values and cannot bypass bounded work.
		if mcpProjectionHasCustomEncoder(value.Type()) {
			return reflect.Value{}, ErrMCPSchemaInvalid
		}
		value = value.Elem()
	}
	return reflect.Value{}, ErrMCPSchemaInvalid
}

// mcpProjectionHasCustomEncoder excludes user-defined serialization from the
// preflight contract, including pointer-receiver methods on scalar aliases.
func mcpProjectionHasCustomEncoder(valueType reflect.Type) bool {
	marshaler := reflect.TypeFor[json.Marshaler]()
	textMarshaler := reflect.TypeFor[encoding.TextMarshaler]()
	pointerType := reflect.PointerTo(valueType)
	return valueType.Implements(marshaler) || valueType.Implements(textMarshaler) || pointerType.Implements(marshaler) || pointerType.Implements(textMarshaler)
}

// admitStruct reads the existing model tags rather than inventing a second
// projection format; arbitrary Example structs fail closed.
func (m *mcpProjectionMeasure) admitStruct(value reflect.Value, depth int) error {
	// Only the owned model has a reviewed, method-free struct serialization shape.
	if value.Type() != reflect.TypeFor[models.Schema]() && value.Type() != reflect.TypeFor[fusedobject.Schema]() {
		return ErrMCPSchemaInvalid
	}
	members := mcpProjectionMembers(value)
	// Object members reserve both their field-name token and their value token.
	if err := m.beginContainer(depth, len(members), 2); err != nil {
		return err
	}
	for _, member := range members {
		// Child admission remains iterative even when Items points to another projection.
		if err := m.admitMember(member.name, member.value, depth); err != nil {
			return err
		}
	}
	return nil
}

// mcpProjectionMembers applies the model's JSON tags and omitempty semantics
// to its fixed-size field list before any dynamic children are queued.
func mcpProjectionMembers(value reflect.Value) []mcpProjectionMember {
	members := make([]mcpProjectionMember, 0, value.NumField())
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index)
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		// Ignored fields do not enter the serialized projection or its budgets.
		if name == "-" {
			continue
		}
		// Untagged future model fields follow encoding/json's ordinary field-name rule.
		if name == "" {
			name = field.Name
		}
		child := value.Field(index)
		// Empty optional fields contribute no encoded bytes or nodes.
		if strings.Contains(options, "omitempty") && mcpProjectionEmpty(child) {
			continue
		}
		members = append(members, mcpProjectionMember{name: name, value: child})
	}
	return members
}

// mcpProjectionEmpty matches encoding/json omitempty for the reviewed model's
// strings, collections, pointers, and interface-valued examples.
func mcpProjectionEmpty(value reflect.Value) bool {
	// Empty collection lengths differ from reflect.IsZero for allocated empty slices/maps.
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	default:
		return value.IsZero()
	}
}

// admitMap bounds width before iterating or queuing children, including maps
// that recursively reference themselves through Example values.
func (m *mcpProjectionMeasure) admitMap(value reflect.Value, depth int) error {
	// Nil maps encode as null rather than consuming a container depth level.
	if value.IsNil() {
		return m.addBytes(4)
	}
	// Persisted JSON examples have string keys; custom key encoders are not a safe fallback.
	if value.Type().Key().Kind() != reflect.String || mcpProjectionHasCustomEncoder(value.Type().Key()) {
		return ErrMCPSchemaInvalid
	}
	if err := m.beginContainer(depth, value.Len(), 2); err != nil {
		return err
	}
	iterator := value.MapRange()
	// MapRange avoids allocating a key slice before the width ceiling is known.
	for iterator.Next() {
		if err := m.admitMember(iterator.Key().String(), iterator.Value(), depth); err != nil {
			return err
		}
	}
	return nil
}

// admitArray reserves the complete child count before iteration, while byte
// slices retain encoding/json's base64-string representation.
func (m *mcpProjectionMeasure) admitArray(value reflect.Value, depth int) error {
	// Nil slices encode as null and contain no traversable child values.
	if value.Kind() == reflect.Slice && value.IsNil() {
		return m.addBytes(4)
	}
	// Plain byte slices are scalar JSON strings, not arrays of numeric nodes.
	if value.Kind() == reflect.Slice && value.Type().Elem().Kind() == reflect.Uint8 && !mcpProjectionHasCustomEncoder(value.Type().Elem()) {
		return m.admitByteSlice(value.Len())
	}
	if err := m.beginContainer(depth, value.Len(), 1); err != nil {
		return err
	}
	for index := 0; index < value.Len(); index++ {
		// The complete child width was reserved before the first stack append.
		m.nodes++
		m.pending = append(m.pending, mcpProjectionWork{value: value.Index(index), depth: depth + 1})
	}
	return nil
}

// admitByteSlice computes base64 growth only after a byte ceiling prevents
// overflow and keeps large binary examples away from the JSON encoder.
func (m *mcpProjectionMeasure) admitByteSlice(length int) error {
	// Encoded base64 can only grow, so oversized input cannot be made admissible.
	if length > maxMCPSchemaEncodedBytes {
		return ErrMCPSchemaEncodedBytesLimit
	}
	return m.addBytes(2 + base64.StdEncoding.EncodedLen(length))
}

// beginContainer checks depth and child reservations before any allocation
// proportional to an attacker-controlled map or slice width.
func (m *mcpProjectionMeasure) beginContainer(depth, children, nodesPerChild int) error {
	// Container depth, not scalar leaf position, matches the raw token scanner.
	if depth > maxMCPSchemaDepth {
		return ErrMCPSchemaDepthLimit
	}
	// Division avoids overflowing while reserving key/value pairs for object members.
	if children > (maxMCPSchemaNodes-m.nodes)/nodesPerChild {
		return ErrMCPSchemaNodeLimit
	}
	return m.addBytes(2 + max(0, children-1))
}

// admitMember charges encoded field names and queues the value under the
// container's already-checked key/value node reservation.
func (m *mcpProjectionMeasure) admitMember(name string, value reflect.Value, depth int) error {
	// Field names can themselves be huge, so they use the same bounded scalar encoder.
	if err := m.admitScalar(reflect.ValueOf(name)); err != nil {
		return err
	}
	// The colon belongs to the object envelope, independent of the child value shape.
	if err := m.addBytes(1); err != nil {
		return err
	}
	m.nodes += 2
	m.pending = append(m.pending, mcpProjectionWork{value: value, depth: depth + 1})
	return nil
}

// admitScalar invokes the standard encoder only for length-admitted primitive
// values, so exact escaping is reused without allowing unbounded allocation.
func (m *mcpProjectionMeasure) admitScalar(value reflect.Value) error {
	// Strings and json.Number tokens must fit even before encoding can expand them.
	if value.Kind() == reflect.String && value.Len() > maxMCPSchemaEncodedBytes-m.encodedBytes {
		return ErrMCPSchemaEncodedBytesLimit
	}
	// These primitive kinds have bounded standard-library encoders and no custom methods.
	switch value.Kind() {
	case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64:
	default:
		return ErrMCPSchemaInvalid
	}
	encoded, err := json.Marshal(value.Interface())
	// Non-JSON numbers, such as NaN or malformed json.Number values, fail without raw detail.
	if err != nil {
		return ErrMCPSchemaInvalid
	}
	return m.addBytes(len(encoded))
}

// admitRawExample bounds RawMessage before its standard-library compaction
// and HTML escaping, then accounts for its embedded structural depth.
func (m *mcpProjectionMeasure) admitRawExample(raw json.RawMessage, depth int) error {
	// Nil RawMessage follows encoding/json's null representation.
	if raw == nil {
		return m.addBytes(4)
	}
	if len(raw) > maxMCPSchemaEncodedBytes-m.encodedBytes {
		return ErrMCPSchemaEncodedBytesLimit
	}
	nodes, err := measureMCPJSONSchema(raw, depth-1)
	// Raw input is checked structurally before even its bounded standard encoder runs.
	if err != nil {
		return err
	}
	// The queued raw value already reserved its root node.
	if nodes-1 > maxMCPSchemaNodes-m.nodes {
		return ErrMCPSchemaNodeLimit
	}
	encoded, err := json.Marshal(raw)
	// Only validated standard RawMessage encoding is admitted, never arbitrary MarshalJSON methods.
	if err != nil {
		return ErrMCPSchemaInvalid
	}
	m.nodes += nodes - 1
	return m.addBytes(len(encoded))
}

// addBytes accumulates exact encoded costs without overflowing or retaining
// any serialized projection content after the size check.
func (m *mcpProjectionMeasure) addBytes(count int) error {
	// Subtraction ensures an untrusted count cannot overflow the aggregate byte sum.
	if count > maxMCPSchemaEncodedBytes-m.encodedBytes {
		return ErrMCPSchemaEncodedBytesLimit
	}
	m.encodedBytes += count
	return nil
}
