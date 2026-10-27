package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

var bridgeSelectorID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)

func (ref BridgeReference) Grouped() bool {
	return ref.Bridge != "" || ref.Profile != "" || ref.Connections != nil
}

// DecodeBridgeOperations distinguishes a wildcard from an explicit finite list.
func DecodeBridgeOperations(raw json.RawMessage) ([]string, bool, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte(`"*"`)) {
		return nil, true, nil
	}
	var operations []string
	if json.Unmarshal(raw, &operations) != nil || len(operations) == 0 || len(operations) > 100 {
		return nil, false, errors.New("operations must be a nonempty list or \"*\"")
	}
	seen := map[string]bool{}
	for _, op := range operations {
		if !bridgeSelectorID.MatchString(op) || seen[op] {
			return nil, false, fmt.Errorf("invalid or duplicate operation %q", op)
		}
		seen[op] = true
	}
	return operations, false, nil
}

// ValidateBridgeGroup keeps readable destinations separate from exact legacy pins.
func ValidateBridgeGroup(ref BridgeReference) error {
	if strings.TrimSpace(ref.Bridge) == "" || len(ref.Bridge) > 128 || strings.ContainsFunc(ref.Bridge, unicode.IsControl) || !bridgeSelectorID.MatchString(ref.Profile) || len(ref.Connections) == 0 || len(ref.Connections) > 100 {
		return errors.New("Bridge group requires bridge name, profile and 1 to 100 connections")
	}
	if ref.PairingID != "" || ref.ProfileID != "" || ref.ManifestHash != "" || ref.ConnectionID != "" || ref.OperationID != "" {
		return errors.New("Bridge group cannot mix readable selectors with explicit pins")
	}
	for name, conn := range ref.Connections {
		if !bridgeSelectorID.MatchString(name) {
			return fmt.Errorf("invalid Bridge connection name %q", name)
		}
		if _, _, err := DecodeBridgeOperations(conn.Operations); err != nil {
			return fmt.Errorf("connection %s: %w", name, err)
		}
	}
	return nil
}
