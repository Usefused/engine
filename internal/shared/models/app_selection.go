package models

import (
	"errors"

	"github.com/Usefused/engine/internal/shared/strictjson"
	"github.com/google/uuid"
)

var ErrAppSelectionSchemaMismatch = errors.New("app_selection_schema_version_mismatch")

// ValidateAppSelections keeps every persisted app consumer on one exact
// selection contract instead of letting a missing field decode as a legacy zero.
func ValidateAppSelections(scopeSchemaVersion int, selections []SDKSelection) error {
	if scopeSchemaVersion != AppScopeSchemaVersion || len(selections) == 0 {
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
