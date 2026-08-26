// Package schemacontract owns Engine-side validation of Registry schema envelopes.
package schemacontract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const (
	MaxDiagnostics   = 128
	MaxDialectLength = 2_048
	MaxCodeLength    = 128
	MaxKeywordLength = 128
	MaxPointerLength = 2_048
	MaxMessageLength = 512
	MaxEncodingDepth = 8
)

// Validate independently verifies the authoritative raw/hash envelope before
// Engine persistence or execution trusts Registry-owned schema metadata.
func Validate(contract *fusedobject.SchemaContract) error {
	// Omitted schemas do not imply any executable schema semantics.
	if contract == nil {
		return nil
	}
	// Dialect metadata is bounded independently from raw JSON size.
	if !validText(contract.Dialect, MaxDialectLength, false) {
		return errors.New("schema contract dialect is invalid")
	}
	decoded, err := hex.DecodeString(contract.ContentHash)
	// Hash spelling is part of the canonical envelope identity, not a case-insensitive identifier.
	if err != nil || len(decoded) != sha256.Size || contract.ContentHash != strings.ToLower(contract.ContentHash) {
		return errors.New("schema contract content hash is invalid")
	}
	expected, err := canonicaljson.HexSchemaSHA256(contract.Raw)
	// Registry and Engine admit the same schema profile without widening execution request limits.
	if err != nil {
		return errors.New("schema contract raw value is invalid")
	}
	// Raw truth must match its independently computed hash before dictionary lookup can execute it.
	if contract.ContentHash != expected {
		return errors.New("schema contract content hash does not match raw schema")
	}
	// Compact roots are trustworthy only when every edge resolves in the admitted version dictionary.
	if err := contract.DefinitionIndex.Validate(contract.Raw, contract.SharedDefinitions); err != nil {
		return err
	}
	return validateDiagnostics(contract.ProjectionDiagnostics)
}

// ValidateParameterContent covers schema envelopes hidden inside encoding
// headers even when a later runtime rule rejects that encoding shape.
func ValidateParameterContent(content fusedobject.ParameterContent, depth int) error {
	if depth >= MaxEncodingDepth {
		return errors.New("schema contract encoding depth is invalid")
	}
	if err := Validate(content.Schema); err != nil {
		return err
	}
	if err := Validate(content.ItemSchema); err != nil {
		return err
	}
	if err := validateEncodingMap(content.Encoding, depth+1); err != nil {
		return err
	}
	if err := validateEncodingSlice(content.PrefixEncoding, depth+1); err != nil {
		return err
	}
	return validateEncodingPointer(content.ItemEncoding, depth+1)
}

func validateEncodingMap(values map[string]fusedobject.RequestEncoding, depth int) error {
	for _, value := range values {
		if err := validateEncoding(value, depth); err != nil {
			return err
		}
	}
	return nil
}

func validateEncodingSlice(values []fusedobject.RequestEncoding, depth int) error {
	for _, value := range values {
		if err := validateEncoding(value, depth); err != nil {
			return err
		}
	}
	return nil
}

func validateEncodingPointer(value *fusedobject.RequestEncoding, depth int) error {
	if value == nil {
		return nil
	}
	return validateEncoding(*value, depth)
}

func validateEncoding(value fusedobject.RequestEncoding, depth int) error {
	if depth >= MaxEncodingDepth {
		return errors.New("schema contract encoding depth is invalid")
	}
	for _, header := range value.Headers {
		if err := validateHeader(header, depth+1); err != nil {
			return err
		}
	}
	if err := validateEncodingMap(value.Encoding, depth+1); err != nil {
		return err
	}
	if err := validateEncodingSlice(value.PrefixEncoding, depth+1); err != nil {
		return err
	}
	return validateEncodingPointer(value.ItemEncoding, depth+1)
}

func validateHeader(header fusedobject.HeaderContract, depth int) error {
	if err := Validate(header.Schema); err != nil {
		return err
	}
	for _, content := range header.Content {
		if err := ValidateParameterContent(content, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func validateDiagnostics(values []fusedobject.SchemaProjectionDiagnostic) error {
	if len(values) > MaxDiagnostics {
		return fmt.Errorf("schema contract diagnostics exceed %d", MaxDiagnostics)
	}
	for _, value := range values {
		if !validDiagnostic(value) {
			return errors.New("schema contract diagnostic is invalid")
		}
	}
	return nil
}

func validDiagnostic(value fusedobject.SchemaProjectionDiagnostic) bool {
	return validText(value.Code, MaxCodeLength, false) &&
		validText(value.Keyword, MaxKeywordLength, true) &&
		validText(value.Pointer, MaxPointerLength, false) &&
		validText(value.Message, MaxMessageLength, false)
}

func validText(value string, maximum int, optional bool) bool {
	if value == "" {
		return optional
	}
	return len(value) <= maximum && !strings.ContainsAny(value, "\r\n\x00")
}
