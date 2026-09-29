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
	Name                       string `json:"name"`
	Title                      string `json:"title"`
	Version                    string `json:"version"`
	Description                string `json:"description"`
	FusedIntelligentClassifier bool   `json:"fused-intelligent-classifier,omitempty"`
}

// Fixture is the app-scoped operation catalogue serialized for the shared MCP runtime.
type Fixture struct {
	Server FixtureServerMetadata `json:"server"`
	// Request-local classifier output is never persisted or supplied by an MCP caller.
	ClassifierOperationNames *[]string `json:"classifier_operation_names,omitempty"`
	// Version-keyed dictionaries are serialized once for lazy schema documentation lookup.
	SchemaDefinitions map[string]map[string]fusedobject.SchemaContract `json:"schema_definitions,omitempty"`
	Operations        []FixtureOperation                               `json:"operations"`

	// byOperationID is built once so repeated tool calls do not scan the app's
	// complete selected operation set.
	byOperationID        map[string]*FixtureOperation
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
