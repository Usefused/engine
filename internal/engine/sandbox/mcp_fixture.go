package sandbox

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// FixtureOperation is the bounded MCP view of an app-scoped operation. It
// intentionally reuses canonical execution-contract types so search_docs and
// call validation cannot drift into a second request or response schema.
type FixtureOperation struct {
	ServiceVersionID string                 `json:"service_version_id,omitempty"`
	OperationID      string                 `json:"operation_id"`
	ServiceID        string                 `json:"service_id"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Method           string                 `json:"method"`
	Path             string                 `json:"path"`
	Parameters       []models.Parameter     `json:"parameters"`
	RequestContent   *models.RequestContent `json:"request_content,omitempty"`
	Responses        models.Responses       `json:"responses"`
	Pagination       FixturePagination      `json:"pagination"`
}

// FixturePagination exposes only the caller controls needed to invoke an operation safely.
type FixturePagination struct {
	Supported            bool `json:"supported"`
	CallerBoundSupported bool `json:"caller_bound_supported"`
	EngineMaxPages       int  `json:"engine_max_pages,omitempty"`
}

// FixtureServerMetadata is the immutable MCP identity advertised before a host inspects any tools.
type FixtureServerMetadata struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// Fixture is the app-scoped operation catalogue serialized for the shared MCP runtime.
type Fixture struct {
	Server FixtureServerMetadata `json:"server"`
	// Version-keyed dictionaries are serialized once for lazy schema documentation lookup.
	SchemaDefinitions map[string]map[string]fusedobject.SchemaContract `json:"schema_definitions,omitempty"`
	Operations        []FixtureOperation                               `json:"operations"`
	UnifiedOperations *models.SDKUnifiedOperationDescriptors           `json:"unified_operations,omitempty"`

	// byOperationID is built once so repeated tool calls do not scan the app's
	// complete selected operation set.
	byOperationID        map[string]*FixtureOperation
	byUnifiedOperationID map[string]*models.SDKUnifiedOperationDescriptor
}

// LoadFixture reads a serialized catalogue for contract tests and offline
// validation. Live MCP sessions build the same shape from immutable local
// snapshots, which keeps execution independent from Registry availability.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fixture: %w", err)
	}

	var f Fixture
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse fixture: %w", err)
	}
	// Offline fixtures cross the same catalogue boundary as live sessions and
	// must fail before unsafe schemas are indexed or exposed to the runtime.
	if err := validateMCPFixtureSchemas(&f); err != nil {
		return nil, fmt.Errorf("admit fixture schemas: %w", err)
	}

	f.byOperationID = make(map[string]*FixtureOperation, len(f.Operations))
	for i := range f.Operations {
		op := &f.Operations[i]
		if op.OperationID == "" {
			return nil, fmt.Errorf("fixture operation at index %d has no operation_id", i)
		}
		if _, exists := f.byOperationID[op.OperationID]; exists {
			return nil, fmt.Errorf("duplicate operation_id %q in fixture", op.OperationID)
		}
		f.byOperationID[op.OperationID] = op
	}
	// Exact cross-kind collisions are ambiguous to call(operationId), so the
	// session must fail before either operation becomes discoverable.
	if err := f.attachUnifiedOperations(f.UnifiedOperations); err != nil {
		return nil, err
	}
	server, err := validateMCPServerMetadata(f.Server)
	// Serialized fixtures are runnable session inputs, so incomplete server identity fails at the Go boundary too.
	if err != nil {
		return nil, fmt.Errorf("admit fixture server metadata: %w", err)
	}
	f.Server = server

	return &f, nil
}

// Resolve uses only the app-scoped catalogue; an unknown operation cannot fall
// through to broader Registry or provider discovery.
func (f *Fixture) Resolve(operationID string) (*FixtureOperation, bool) {
	// A missing fixture cannot authorize a fallback outside the session catalogue.
	if f == nil {
		return nil, false
	}
	op, ok := f.byOperationID[operationID]
	return op, ok
}

// ResolveUnified uses the same exact authored name exposed by search_docs so
// dispatch never scans descriptors or infers kind from the invocation shape.
func (f *Fixture) ResolveUnified(operationID string) (*models.SDKUnifiedOperationDescriptor, bool) {
	// A missing fixture cannot authorize a fallback to private runtime state.
	if f == nil {
		return nil, false
	}
	operation, ok := f.byUnifiedOperationID[operationID]
	return operation, ok
}

// attachUnifiedOperations validates and attaches the existing public descriptor
// only when call(operationId) can classify every exact name unambiguously.
func (f *Fixture) attachUnifiedOperations(descriptors *models.SDKUnifiedOperationDescriptors) error {
	// An absent descriptor is the canonical empty logical catalogue.
	if descriptors == nil {
		f.UnifiedOperations = nil
		f.byUnifiedOperationID = nil
		return nil
	}
	// Runtime fixtures accept only the shared compiler-owned descriptor schema.
	if descriptors.SchemaVersion != models.SDKUnifiedDescriptorSchemaVersion {
		return fmt.Errorf("unsupported Unified descriptor schema version %d", descriptors.SchemaVersion)
	}
	indexed := make(map[string]*models.SDKUnifiedOperationDescriptor, len(descriptors.Operations))
	for position := range descriptors.Operations {
		operation := &descriptors.Operations[position]
		// Empty, repeated, or cross-kind names would make exact dispatch depend on
		// construction order, so all three fail before the session starts.
		if operation.Name == "" {
			return fmt.Errorf("Unified descriptor operation at index %d has no name", position)
		}
		if _, duplicate := indexed[operation.Name]; duplicate {
			return fmt.Errorf("duplicate Unified operation name %q", operation.Name)
		}
		if _, collision := f.byOperationID[operation.Name]; collision {
			return fmt.Errorf("physical and Unified operation name collision %q", operation.Name)
		}
		indexed[operation.Name] = operation
	}
	f.UnifiedOperations = descriptors
	f.byUnifiedOperationID = indexed
	return nil
}
