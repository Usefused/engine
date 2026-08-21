package unified

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/google/uuid"
)

const (
	maxDefinitionOperations       = 64
	maxDefinitionBindings         = 16
	maxDefinitionNameBytes        = 256
	maxDefinitionDescriptionBytes = 4 << 10
	maxDefinitionOperationIDBytes = 512
	maxDefinitionTargetBytes      = 253
)

var definitionNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

type wireOperationDefinition struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	InputSchema json.RawMessage         `json:"input_schema"`
	Bindings    []wireBindingDefinition `json:"bindings"`
	Output      *wireOutputDefinition   `json:"output,omitempty"`
}

type wireBindingDefinition struct {
	PublicTarget     string                  `json:"public_target"`
	ServiceTarget    string                  `json:"service_target,omitempty"`
	OperationID      string                  `json:"operation_id"`
	ServiceID        string                  `json:"service_id"`
	ServiceVersionID string                  `json:"service_version_id"`
	EndpointID       string                  `json:"endpoint_id"`
	DependsOn        []string                `json:"depends_on,omitempty"`
	Input            json.RawMessage         `json:"input,omitempty"`
	Output           *wireOutputDefinition   `json:"output,omitempty"`
	Rollback         *wireRollbackDefinition `json:"rollback,omitempty"`
}

// wireRollbackDefinition is the strict persisted form of an exact rollback.
type wireRollbackDefinition struct {
	OperationID      string          `json:"operation_id"`
	ServiceID        string          `json:"service_id"`
	ServiceVersionID string          `json:"service_version_id"`
	EndpointID       string          `json:"endpoint_id"`
	Input            json.RawMessage `json:"input,omitempty"`
}

type wireOutputDefinition struct {
	Schema  json.RawMessage `json:"schema"`
	Mapping json.RawMessage `json:"mapping"`
}

// EncodeDefinitions produces the canonical private JSON array persisted for
// one immutable app version. Programs use their own versioned wire codec.
func EncodeDefinitions(definitions []OperationDefinition, limits Limits) ([]byte, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	if err := validateDefinitionCount(len(definitions)); err != nil {
		return nil, err
	}
	ordered := append([]OperationDefinition(nil), definitions...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	encoded := make([]wireOperationDefinition, 0, len(ordered))
	for index, definition := range ordered {
		if index > 0 && ordered[index-1].Name == definition.Name {
			return nil, definitionError("duplicate operation name", nil)
		}
		wire, err := encodeDefinition(definition, limits)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, wire)
	}
	return marshalCanonicalDefinitions(encoded, limits.MaxEncodedBytes)
}

// DecodeDefinitions strictly rehydrates private definitions. Each program is
// admitted only against the exact public targets in its containing operation.
func DecodeDefinitions(encoded []byte, limits Limits) ([]OperationDefinition, error) {
	if err := validateLimits(limits); err != nil {
		return nil, err
	}
	canonical, err := canonicalDefinitionArray(encoded, limits.MaxEncodedBytes)
	if err != nil {
		return nil, err
	}
	var wire []wireOperationDefinition
	if err := decodeStrictJSON(canonical, &wire); err != nil {
		return nil, definitionError("decode", err)
	}
	if err := validateDefinitionCount(len(wire)); err != nil {
		return nil, err
	}
	sort.Slice(wire, func(i, j int) bool { return wire[i].Name < wire[j].Name })
	definitions := make([]OperationDefinition, 0, len(wire))
	for index, persisted := range wire {
		if index > 0 && wire[index-1].Name == persisted.Name {
			return nil, definitionError("duplicate operation name", nil)
		}
		definition, err := decodeDefinition(persisted, limits)
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// encodeDefinition serializes definition into canonical private bytes for stable hashing.
func encodeDefinition(value OperationDefinition, limits Limits) (wireOperationDefinition, error) {
	if err := validateDefinitionMetadata(value.Name, value.Description); err != nil {
		return wireOperationDefinition{}, err
	}
	inputSchema, err := canonicalSchemaObject(value.InputSchema)
	if err != nil {
		return wireOperationDefinition{}, err
	}
	bindings, targets, err := prepareBindingsForEncode(value.Bindings)
	if err != nil {
		return wireOperationDefinition{}, err
	}
	// Graph admission belongs to the immutable definition boundary, so
	// corrupted persisted state cannot bypass plan-time dependency validation.
	if err := validateBindingGraph(bindings); err != nil {
		return wireOperationDefinition{}, err
	}
	encodedBindings, err := encodeBindings(bindings, targets, limits)
	if err != nil {
		return wireOperationDefinition{}, err
	}
	output, err := encodeDefinitionOutput(value.Output, targets, limits)
	if err != nil {
		return wireOperationDefinition{}, err
	}
	return wireOperationDefinition{
		Name: value.Name, Description: value.Description, InputSchema: inputSchema,
		Bindings: encodedBindings, Output: output,
	}, nil
}

// decodeDefinition restores definition only after strict shape, limit, and namespace checks.
func decodeDefinition(value wireOperationDefinition, limits Limits) (OperationDefinition, error) {
	if err := validateDefinitionMetadata(value.Name, value.Description); err != nil {
		return OperationDefinition{}, err
	}
	inputSchema, err := canonicalSchemaObject(value.InputSchema)
	if err != nil {
		return OperationDefinition{}, err
	}
	bindings, targets, err := prepareBindingsForDecode(value.Bindings)
	if err != nil {
		return OperationDefinition{}, err
	}
	decodedBindings, err := decodeBindings(bindings, targets, limits)
	if err != nil {
		return OperationDefinition{}, err
	}
	// Decoding proves the stored graph remains acyclic and exact before it
	// can influence runtime scheduling.
	if err := validateBindingGraph(decodedBindings); err != nil {
		return OperationDefinition{}, err
	}
	output, err := decodeDefinitionOutput(value.Output, targets, limits)
	if err != nil {
		return OperationDefinition{}, err
	}
	return OperationDefinition{
		Name: value.Name, Description: value.Description, InputSchema: inputSchema,
		Bindings: decodedBindings, Output: output,
	}, nil
}

// prepareBindingsForEncode assembles bindings for encode without starting persistence, accounting, or provider work.
func prepareBindingsForEncode(values []BindingDefinition) ([]BindingDefinition, []string, error) {
	if err := validateBindingCount(len(values)); err != nil {
		return nil, nil, err
	}
	ordered := append([]BindingDefinition(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PublicTarget < ordered[j].PublicTarget })
	targets := make([]string, 0, len(ordered))
	for index, value := range ordered {
		if err := admitDefinitionTarget(value.PublicTarget, targets, index); err != nil {
			return nil, nil, err
		}
		targets = append(targets, value.PublicTarget)
	}
	return ordered, targets, nil
}

// prepareBindingsForDecode assembles bindings for decode without starting persistence, accounting, or provider work.
func prepareBindingsForDecode(values []wireBindingDefinition) ([]wireBindingDefinition, []string, error) {
	if err := validateBindingCount(len(values)); err != nil {
		return nil, nil, err
	}
	ordered := append([]wireBindingDefinition(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].PublicTarget < ordered[j].PublicTarget })
	targets := make([]string, 0, len(ordered))
	for index, value := range ordered {
		if err := admitDefinitionTarget(value.PublicTarget, targets, index); err != nil {
			return nil, nil, err
		}
		targets = append(targets, value.PublicTarget)
	}
	return ordered, targets, nil
}

// admitDefinitionTarget charges shared limits and rejects unsupported immutable Unified definitions shapes before allocation grows.
func admitDefinitionTarget(target string, previous []string, index int) error {
	if target == "" || len(target) > maxDefinitionTargetBytes || strings.TrimSpace(target) != target {
		return definitionError("invalid public target", nil)
	}
	if index > 0 && previous[index-1] == target {
		return definitionError("duplicate public target", nil)
	}
	return nil
}

// encodeBindings serializes bindings into canonical private bytes for stable hashing.
func encodeBindings(values []BindingDefinition, targets []string, limits Limits) ([]wireBindingDefinition, error) {
	encoded := make([]wireBindingDefinition, 0, len(values))
	for _, value := range values {
		binding, err := encodeBinding(value, targets, limits)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, binding)
	}
	return encoded, nil
}

// encodeBinding serializes binding into canonical private bytes for stable hashing.
func encodeBinding(value BindingDefinition, targets []string, limits Limits) (wireBindingDefinition, error) {
	if err := validateBindingMetadata(value.OperationID, value.ServiceID, value.ServiceVersionID, value.EndpointID); err != nil {
		return wireBindingDefinition{}, err
	}
	serviceTarget, err := canonicalServiceTarget(value.PublicTarget, value.ServiceTarget)
	if err != nil {
		return wireBindingDefinition{}, err
	}
	dependsOn, err := canonicalDependencies(value.PublicTarget, value.DependsOn, targets)
	if err != nil {
		return wireBindingDefinition{}, err
	}
	// A dependent input may read exactly the responses it explicitly waits
	// for, which keeps dataflow and scheduling authority in the same declaration.
	input, err := encodeOptionalDefinitionProgram(value.Input, limits, dependsOn)
	if err != nil {
		return wireBindingDefinition{}, err
	}
	output, err := encodeDefinitionOutput(value.Output, []string{value.PublicTarget}, limits)
	if err != nil {
		return wireBindingDefinition{}, err
	}
	rollback, err := encodeRollbackDefinition(value.PublicTarget, value.Rollback, limits)
	if err != nil {
		return wireBindingDefinition{}, err
	}
	return wireBindingDefinition{
		PublicTarget: value.PublicTarget, ServiceTarget: serviceTarget, OperationID: value.OperationID,
		ServiceID: value.ServiceID.String(), ServiceVersionID: value.ServiceVersionID.String(),
		EndpointID: value.EndpointID.String(), DependsOn: dependsOn, Input: input,
		Output: output, Rollback: rollback,
	}, nil
}

// decodeBindings restores bindings only after strict shape, limit, and namespace checks.
func decodeBindings(values []wireBindingDefinition, targets []string, limits Limits) ([]BindingDefinition, error) {
	decoded := make([]BindingDefinition, 0, len(values))
	for _, value := range values {
		binding, err := decodeBinding(value, targets, limits)
		if err != nil {
			return nil, err
		}
		decoded = append(decoded, binding)
	}
	return decoded, nil
}

// decodeBinding restores binding only after strict shape, limit, and namespace checks.
func decodeBinding(value wireBindingDefinition, targets []string, limits Limits) (BindingDefinition, error) {
	identities, err := decodeBindingIdentities(value)
	if err != nil {
		return BindingDefinition{}, err
	}
	serviceTarget, err := canonicalServiceTarget(value.PublicTarget, value.ServiceTarget)
	if err != nil {
		return BindingDefinition{}, err
	}
	dependsOn, err := canonicalDependencies(value.PublicTarget, value.DependsOn, targets)
	if err != nil {
		return BindingDefinition{}, err
	}
	input, err := decodeOptionalDefinitionProgram(value.Input, limits, dependsOn)
	if err != nil {
		return BindingDefinition{}, err
	}
	output, err := decodeDefinitionOutput(value.Output, []string{value.PublicTarget}, limits)
	if err != nil {
		return BindingDefinition{}, err
	}
	rollback, err := decodeRollbackDefinition(value.PublicTarget, value.Rollback, limits)
	if err != nil {
		return BindingDefinition{}, err
	}
	return BindingDefinition{
		PublicTarget: value.PublicTarget, ServiceTarget: serviceTarget, OperationID: value.OperationID,
		ServiceID: identities[0], ServiceVersionID: identities[1], EndpointID: identities[2],
		DependsOn: dependsOn, Input: input, Output: output, Rollback: rollback,
	}, nil
}

// canonicalServiceTarget applies the public-target default while keeping an
// explicit selector namespace exact and bounded.
func canonicalServiceTarget(publicTarget, serviceTarget string) (string, error) {
	// Omitted service targets intentionally inherit the validated graph target.
	if serviceTarget == "" {
		return publicTarget, nil
	}
	// Exact bounded names prevent persisted selector namespaces from being normalized differently at runtime.
	if len(serviceTarget) > maxDefinitionTargetBytes || strings.TrimSpace(serviceTarget) != serviceTarget {
		return "", definitionError("invalid service target", nil)
	}
	return serviceTarget, nil
}

// encodeRollbackDefinition validates and serializes one same-binding rollback.
func encodeRollbackDefinition(target string, value *RollbackDefinition, limits Limits) (*wireRollbackDefinition, error) {
	if value == nil {
		return nil, nil
	}
	if err := validateBindingMetadata(value.OperationID, value.ServiceID, value.ServiceVersionID, value.EndpointID); err != nil {
		return nil, err
	}
	input, err := encodeOptionalDefinitionProgram(value.Input, limits, []string{target})
	if err != nil {
		return nil, err
	}
	return &wireRollbackDefinition{
		OperationID: value.OperationID, ServiceID: value.ServiceID.String(),
		ServiceVersionID: value.ServiceVersionID.String(), EndpointID: value.EndpointID.String(), Input: input,
	}, nil
}

// decodeRollbackDefinition strictly restores one exact rollback operation.
func decodeRollbackDefinition(target string, value *wireRollbackDefinition, limits Limits) (*RollbackDefinition, error) {
	if value == nil {
		return nil, nil
	}
	identities, err := decodeRollbackIdentities(value)
	if err != nil {
		return nil, err
	}
	input, err := decodeOptionalDefinitionProgram(value.Input, limits, []string{target})
	if err != nil {
		return nil, err
	}
	return &RollbackDefinition{
		OperationID: value.OperationID, ServiceID: identities[0],
		ServiceVersionID: identities[1], EndpointID: identities[2], Input: input,
	}, nil
}

// decodeRollbackIdentities validates canonical UUID strings without widening
// the rollback operation selected during plan.
func decodeRollbackIdentities(value *wireRollbackDefinition) ([3]uuid.UUID, error) {
	if err := validateOperationID(value.OperationID); err != nil {
		return [3]uuid.UUID{}, err
	}
	return decodeDefinitionUUIDs(value.ServiceID, value.ServiceVersionID, value.EndpointID)
}

// decodeBindingIdentities restores binding identities only after strict shape, limit, and namespace checks.
func decodeBindingIdentities(value wireBindingDefinition) ([3]uuid.UUID, error) {
	if err := validateOperationID(value.OperationID); err != nil {
		return [3]uuid.UUID{}, err
	}
	return decodeDefinitionUUIDs(value.ServiceID, value.ServiceVersionID, value.EndpointID)
}

// decodeDefinitionUUIDs validates the three exact identities shared by
// forward and rollback definitions.
func decodeDefinitionUUIDs(values ...string) ([3]uuid.UUID, error) {
	if len(values) != 3 {
		return [3]uuid.UUID{}, definitionError("invalid binding identity count", nil)
	}
	var identities [3]uuid.UUID
	for index, raw := range values {
		identity, err := decodeCanonicalUUID(raw)
		if err != nil {
			return [3]uuid.UUID{}, err
		}
		identities[index] = identity
	}
	return identities, nil
}

// encodeDefinitionOutput serializes definition output into canonical private bytes for stable hashing.
func encodeDefinitionOutput(value *OutputDefinition, targets []string, limits Limits) (*wireOutputDefinition, error) {
	if value == nil {
		return nil, nil
	}
	schema, err := canonicalSchemaObject(value.Schema)
	if err != nil {
		return nil, err
	}
	mapping, err := encodeRequiredDefinitionProgram(value.Mapping, limits, targets)
	if err != nil {
		return nil, err
	}
	return &wireOutputDefinition{Schema: schema, Mapping: mapping}, nil
}

// decodeDefinitionOutput restores definition output only after strict shape, limit, and namespace checks.
func decodeDefinitionOutput(value *wireOutputDefinition, targets []string, limits Limits) (*OutputDefinition, error) {
	if value == nil {
		return nil, nil
	}
	schema, err := canonicalSchemaObject(value.Schema)
	if err != nil {
		return nil, err
	}
	mapping, err := decodeRequiredDefinitionProgram(value.Mapping, limits, targets)
	if err != nil {
		return nil, err
	}
	return &OutputDefinition{Schema: schema, Mapping: mapping}, nil
}

// encodeOptionalDefinitionProgram serializes optional definition program into canonical private bytes for stable hashing.
func encodeOptionalDefinitionProgram(value *Program, limits Limits, targets []string) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	return encodeRequiredDefinitionProgram(value, limits, targets)
}

// encodeRequiredDefinitionProgram serializes required definition program into canonical private bytes for stable hashing.
func encodeRequiredDefinitionProgram(value *Program, limits Limits, targets []string) (json.RawMessage, error) {
	if value == nil {
		return nil, definitionError("missing compiled mapping", nil)
	}
	encoded, err := EncodeProgram(value, limits)
	if err != nil {
		return nil, definitionError("encode compiled mapping", err)
	}
	// EncodeProgram validates AST shape, while a decode against the exact
	// targets also proves no forged response namespace can enter persistence.
	if _, err := DecodeProgram(encoded, limits, targets); err != nil {
		return nil, definitionError("compiled mapping target", err)
	}
	return encoded, nil
}

// decodeOptionalDefinitionProgram restores optional definition program only after strict shape, limit, and namespace checks.
func decodeOptionalDefinitionProgram(value json.RawMessage, limits Limits, targets []string) (*Program, error) {
	if len(value) == 0 {
		return nil, nil
	}
	return decodeRequiredDefinitionProgram(value, limits, targets)
}

// decodeRequiredDefinitionProgram restores required definition program only after strict shape, limit, and namespace checks.
func decodeRequiredDefinitionProgram(value json.RawMessage, limits Limits, targets []string) (*Program, error) {
	if len(value) == 0 {
		return nil, definitionError("missing compiled mapping", nil)
	}
	program, err := DecodeProgram(value, limits, targets)
	if err != nil {
		return nil, definitionError("decode compiled mapping", err)
	}
	return program, nil
}

// canonicalSchemaObject normalizes immutable Unified definitions ordering and bytes so hashes are reproducible.
func canonicalSchemaObject(value json.RawMessage) (json.RawMessage, error) {
	canonical, err := canonicaljson.Canonicalize(value)
	if err != nil || len(canonical) == 0 || canonical[0] != '{' {
		return nil, definitionError("schema must be one JSON object", err)
	}
	return json.RawMessage(canonical), nil
}

// marshalCanonicalDefinitions serializes canonical definitions into canonical private bytes for stable hashing.
func marshalCanonicalDefinitions(value []wireOperationDefinition, maxBytes int) ([]byte, error) {
	if value == nil {
		value = []wireOperationDefinition{}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, definitionError("encode", err)
	}
	canonical, err := canonicalDefinitionArray(encoded, maxBytes)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

// canonicalDefinitionArray normalizes immutable Unified definitions ordering and bytes so hashes are reproducible.
func canonicalDefinitionArray(value []byte, maxBytes int) ([]byte, error) {
	if len(value) == 0 {
		return nil, definitionError("empty JSON", nil)
	}
	if len(value) > maxBytes {
		return nil, fmt.Errorf("%w: maximum definition size exceeded", ErrLimitExceeded)
	}
	canonical, err := canonicaljson.Canonicalize(value)
	if err != nil || len(canonical) == 0 || canonical[0] != '[' {
		return nil, definitionError("definitions must be one JSON array", err)
	}
	if len(canonical) > maxBytes {
		return nil, fmt.Errorf("%w: maximum definition size exceeded", ErrLimitExceeded)
	}
	return canonical, nil
}

// validateDefinitionCount rejects malformed definition count before it can cross the immutable Unified definitions boundary.
func validateDefinitionCount(count int) error {
	if count > maxDefinitionOperations {
		return fmt.Errorf("%w: maximum operation count exceeded", ErrLimitExceeded)
	}
	return nil
}

// validateBindingCount rejects malformed binding count before it can cross the immutable Unified definitions boundary.
func validateBindingCount(count int) error {
	if count == 0 {
		return definitionError("operation requires a binding", nil)
	}
	if count > maxDefinitionBindings {
		return fmt.Errorf("%w: maximum binding count exceeded", ErrLimitExceeded)
	}
	return nil
}

// validateDefinitionMetadata rejects malformed definition metadata before it can cross the immutable Unified definitions boundary.
func validateDefinitionMetadata(name, description string) error {
	if name == "" || len(name) > maxDefinitionNameBytes || !definitionNamePattern.MatchString(name) {
		return definitionError("invalid operation name", nil)
	}
	if len(description) > maxDefinitionDescriptionBytes {
		return fmt.Errorf("%w: maximum description size exceeded", ErrLimitExceeded)
	}
	return nil
}

// validateBindingMetadata rejects malformed binding metadata before it can cross the immutable Unified definitions boundary.
func validateBindingMetadata(operationID string, identities ...uuid.UUID) error {
	if err := validateOperationID(operationID); err != nil {
		return err
	}
	for _, identity := range identities {
		if identity == uuid.Nil {
			return definitionError("nil binding identity", nil)
		}
	}
	return nil
}

// validateOperationID rejects malformed operation id before it can cross the immutable Unified definitions boundary.
func validateOperationID(value string) error {
	if value == "" || len(value) > maxDefinitionOperationIDBytes || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\x00") {
		return definitionError("invalid operation ID", nil)
	}
	return nil
}

// decodeCanonicalUUID restores canonical uuid only after strict shape, limit, and namespace checks.
func decodeCanonicalUUID(value string) (uuid.UUID, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return uuid.Nil, definitionError("invalid binding UUID", err)
	}
	return parsed, nil
}

// definitionError preserves one sentinel classification while retaining the bounded validation cause.
func definitionError(message string, cause error) error {
	if cause == nil {
		return fmt.Errorf("%w: %s", ErrInvalidDefinitions, message)
	}
	return fmt.Errorf("%w: %s: %w", ErrInvalidDefinitions, message, cause)
}
