package canonicaljson

import (
	"bytes"
	"strings"
	"testing"
)

// TestSchemaProfileCapacityAndHashParity keeps the schema allowance separate from ordinary JSON admission.
func TestSchemaProfileCapacityAndHashParity(t *testing.T) {
	raw := []byte(`{"description":"` + strings.Repeat("x", MaxSchemaInputBytes-len(`{"description":""}`)) + `"}`)
	canonical, err := CanonicalizeSchema(raw)
	// The exact schema ceiling must be usable, not merely an advertised constant.
	if err != nil || !bytes.Equal(canonical, raw) {
		t.Fatalf("schema boundary: bytes=%d err=%v", len(canonical), err)
	}
	// Generic callers, including execution requests, must retain their original cap.
	if _, err := Canonicalize(raw); err == nil {
		t.Fatal("ordinary profile admitted a schema-sized value")
	}
	// One extra whitespace byte exceeds the input budget even though it would canonicalize away.
	if _, err := CanonicalizeSchema(append(raw, ' ')); err == nil {
		t.Fatal("schema profile accepted oversized input")
	}
	// Persisted digests from the original profile must remain byte-for-byte compatible.
	for _, input := range []string{`{"z":1.00,"a":"<&>"}`, `false`, `[1e3,0,-0.0]`} {
		want, err := HexSHA256([]byte(input))
		// The reference profile must itself accept every compatibility vector.
		if err != nil {
			t.Fatal(err)
		}
		got, err := HexSchemaSHA256([]byte(input))
		// Capacity is the only difference; schema identities cannot change.
		if err != nil || got != want {
			t.Fatalf("schema digest %s want %s: %v", got, want, err)
		}
	}
}

// TestSchemaProfileRetainsStructuralGuards prevents the larger byte allowance from bypassing ambiguity or work limits.
func TestSchemaProfileRetainsStructuralGuards(t *testing.T) {
	for _, input := range []string{
		`{"a":1,"\u0061":2}`, `{} {}`, `"\ud800"`,
		strings.Repeat("[", MaxDepth+1) + "0" + strings.Repeat("]", MaxDepth+1),
		"[" + strings.Repeat("0,", MaxValues) + "0]",
	} {
		// Every structural constraint is identical under the schema and ordinary profiles.
		if _, err := CanonicalizeSchema([]byte(input)); err == nil {
			t.Fatal("schema profile accepted invalid or over-budget JSON")
		}
	}
	// HTML-sensitive strings can expand sixfold; canonical output must still fit on subsequent reads.
	if _, err := CanonicalizeSchema([]byte(`"` + strings.Repeat("<", MaxSchemaInputBytes/6+1) + `"`)); err == nil {
		t.Fatal("schema profile accepted oversized canonical output")
	}
}
