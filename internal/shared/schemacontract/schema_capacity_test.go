package schemacontract

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// TestValidateLargeSchemaEnvelope proves independently verified Registry envelopes can cross the old 1 MiB ceiling.
func TestValidateLargeSchemaEnvelope(t *testing.T) {
	raw := []byte(`{"description":"` + strings.Repeat("x", canonicaljson.MaxInputBytes) + `","type":"string"}`)
	hash, err := canonicaljson.HexSchemaSHA256(raw)
	// Fixtures use the producer's schema profile, not a manually trusted hash label.
	if err != nil {
		t.Fatal(err)
	}
	contract := &fusedobject.SchemaContract{Dialect: "https://json-schema.org/draft/2020-12/schema", Raw: raw, ContentHash: hash}
	// A formerly oversized but valid envelope must pass Engine's independent validator.
	if err := Validate(contract); err != nil {
		t.Fatal(err)
	}
	contract.Raw = []byte(`{"type":"string","description":"` + strings.Repeat("x", canonicaljson.MaxInputBytes) + `"}`)
	// jsonb member reordering must not invalidate the content identity.
	if err := Validate(contract); err != nil {
		t.Fatal(err)
	}
	contract.ContentHash = strings.Repeat("0", 64)
	// Increased capacity cannot relax integrity validation.
	if err := Validate(contract); err == nil {
		t.Fatal("forged large schema hash was accepted")
	}
	contract.Raw = []byte(`"` + strings.Repeat("x", canonicaljson.MaxSchemaInputBytes) + `"`)
	// The new schema profile remains bounded at the consumer boundary.
	if err := Validate(contract); err == nil {
		t.Fatal("oversized schema was accepted")
	}
}
