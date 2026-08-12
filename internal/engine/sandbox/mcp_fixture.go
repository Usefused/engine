package sandbox

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Usefused/engine/internal/shared/models"
)

// FixtureOperation is the bounded MCP view of an app-scoped operation. It
// intentionally reuses canonical execution-contract types so search_docs and
// call validation cannot drift into a second request or response schema.
type FixtureOperation struct {
	OperationID    string                 `json:"operation_id"`
	ServiceID      string                 `json:"service_id"`
	Name           string                 `json:"name"`
	Description    string                 `json:"description"`
	Method         string                 `json:"method"`
	Path           string                 `json:"path"`
	Parameters     []models.Parameter     `json:"parameters"`
	RequestContent *models.RequestContent `json:"request_content,omitempty"`
	Responses      models.Responses       `json:"responses"`
}

// Fixture is the app-scoped operation catalogue serialized for the shared MCP runtime.
type Fixture struct {
	Operations []FixtureOperation `json:"operations"`

	// byOperationID is built once so repeated tool calls do not scan the app's
	// complete selected operation set.
	byOperationID map[string]*FixtureOperation
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

	return &f, nil
}

// Resolve uses only the app-scoped catalogue; an unknown operation cannot fall
// through to broader Registry or provider discovery.
func (f *Fixture) Resolve(operationID string) (*FixtureOperation, bool) {
	if f == nil {
		return nil, false
	}
	op, ok := f.byOperationID[operationID]
	return op, ok
}
