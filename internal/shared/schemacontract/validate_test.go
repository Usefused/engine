package schemacontract

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

func TestValidateMatchesRegistryEnvelopeBounds(t *testing.T) {
	contract := validContract(t)
	contract.ProjectionDiagnostics = []fusedobject.SchemaProjectionDiagnostic{{
		Code: "projection", Pointer: "#", Message: strings.Repeat("x", MaxMessageLength+1),
	}}
	if err := Validate(contract); err == nil {
		t.Fatal("Validate accepted an oversized diagnostic")
	}
	contract = validContract(t)
	contract.Dialect = strings.Repeat("x", MaxDialectLength+1)
	if err := Validate(contract); err == nil {
		t.Fatal("Validate accepted an oversized dialect")
	}
}

func TestValidateRejectsNonCanonicalOrMismatchedHash(t *testing.T) {
	contract := validContract(t)
	contract.ContentHash = strings.ToUpper(contract.ContentHash)
	if err := Validate(contract); err == nil {
		t.Fatal("Validate accepted uppercase hash")
	}
	contract = validContract(t)
	contract.Raw = []byte(`{"type":"array"}`)
	if err := Validate(contract); err == nil {
		t.Fatal("Validate accepted mismatched raw schema")
	}
}

func validContract(t *testing.T) *fusedobject.SchemaContract {
	t.Helper()
	raw := []byte(`{"type":"object"}`)
	hash, err := canonicaljson.HexSHA256(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw, ContentHash: hash}
}
