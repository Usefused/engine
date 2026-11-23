package models

import (
	"errors"

	"github.com/Usefused/engine/internal/shared/strictjson"
	"github.com/google/uuid"
)

// AllowsEndpoint reports whether this SDKSelection admits an endpoint by
// select-all, explicit endpoint ID, or canonical operation name so OpenAPI
// projection and runtime governance share one admission predicate.
func (selection SDKSelection) AllowsEndpoint(endpointID uuid.UUID, operationName string) bool {
	// SelectAll grants every endpoint on the pinned service version.
	if selection.SelectAll {
		return true
	}
	for _, allowedID := range selection.EndpointIDs {
		// Match explicit endpoint UUID pins before checking operation names.
		if allowedID == endpointID {
			return true
		}
	}
	for _, allowedName := range selection.OperationNames {
		// Match versioned operation names for name-pinned selections.
		if allowedName == operationName {
			return true
		}
	}
	return false
}

var ErrAppSelectionSchemaMismatch = errors.New("app_selection_schema_version_mismatch")

// ValidateAppSelections keeps every persisted app consumer on one exact
// selection contract while allowing an explicit empty set for authored code-only Apps.
func ValidateAppSelections(scopeSchemaVersion int, selections []SDKSelection) error {
	// Nil means omitted or JSON null; only an explicit [] can represent a code-only scope.
	if scopeSchemaVersion != AppScopeSchemaVersion || selections == nil {
		return ErrAppSelectionSchemaMismatch
	}
	for _, selection := range selections {
		if selection.SchemaVersion != AppSelectionSchemaVersion || selection.ServiceID == uuid.Nil || selection.ServiceVersionID == uuid.Nil {
			return ErrAppSelectionSchemaMismatch
		}
	}
	return nil
}

// DecodeAppSelections validates the wire payload immediately after decoding so
// old field names cannot survive as zero-valued current selections.
func DecodeAppSelections(scopeSchemaVersion int, payload []byte) ([]SDKSelection, error) {
	var selections []SDKSelection
	if len(payload) == 0 || strictjson.Decode(payload, &selections, "app selections") != nil {
		return nil, ErrAppSelectionSchemaMismatch
	}
	if err := ValidateAppSelections(scopeSchemaVersion, selections); err != nil {
		return nil, err
	}
	return selections, nil
}
