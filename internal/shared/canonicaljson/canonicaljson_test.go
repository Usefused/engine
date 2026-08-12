package canonicaljson

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type conformanceFixture struct {
	Version               string `json:"version"`
	MaxInputBytes         int    `json:"max_input_bytes"`
	MaxDepth              int    `json:"max_depth"`
	MaxValues             int    `json:"max_values"`
	MaxNumberDigits       int    `json:"max_number_digits"`
	MaxAbsDecimalExponent int    `json:"max_abs_decimal_exponent"`
	Vectors               []struct {
		Name      string   `json:"name"`
		Inputs    []string `json:"inputs"`
		Canonical string   `json:"canonical"`
		SHA256    string   `json:"sha256"`
	} `json:"vectors"`
	InvalidInputs []string `json:"invalid_inputs"`
}

func TestCanonicalizeConformsToFusedV1Fixture(t *testing.T) {
	fixture := loadConformanceFixture(t)
	if !fixture.matchesImplementation() {
		t.Fatalf("fixture contract %q limits do not match the implementation", fixture.Version)
	}
	for _, vector := range fixture.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			for _, input := range vector.Inputs {
				canonical, err := Canonicalize([]byte(input))
				if err != nil {
					t.Fatalf("Canonicalize(%q): %v", input, err)
				}
				if string(canonical) != vector.Canonical {
					t.Fatalf("Canonicalize(%q) = %q, want %q", input, canonical, vector.Canonical)
				}
				hash, err := HexSHA256([]byte(input))
				if err != nil || hash != vector.SHA256 {
					t.Fatalf("HexSHA256(%q) = %q, want %q: %v", input, hash, vector.SHA256, err)
				}
			}
		})
	}
	for _, input := range fixture.InvalidInputs {
		if _, err := Canonicalize([]byte(input)); err == nil {
			t.Errorf("Canonicalize(%q) accepted invalid JSON", input)
		}
	}
}

func (f conformanceFixture) matchesImplementation() bool {
	return f.Version == Version &&
		f.MaxInputBytes == MaxInputBytes &&
		f.MaxDepth == MaxDepth &&
		f.MaxValues == MaxValues &&
		f.MaxNumberDigits == MaxNumberDigits &&
		f.MaxAbsDecimalExponent == MaxAbsDecimalExponent
}

func TestCanonicalizeRejectsInvalidUTF8AndDepthOverflow(t *testing.T) {
	if _, err := Canonicalize([]byte{'"', 0xff, '"'}); err == nil {
		t.Fatal("Canonicalize accepted invalid UTF-8")
	}
	atLimit := strings.Repeat("[", MaxDepth) + "true" + strings.Repeat("]", MaxDepth)
	if _, err := Canonicalize([]byte(atLimit)); err != nil {
		t.Fatalf("Canonicalize rejected maximum depth: %v", err)
	}
	overLimit := "[" + atLimit + "]"
	if _, err := Canonicalize([]byte(overLimit)); err == nil {
		t.Fatal("Canonicalize accepted depth overflow")
	}
}

func TestCanonicalizeDefinesEveryControlEscape(t *testing.T) {
	input := []byte(`"\u0000\u0001\u0002\u0003\u0004\u0005\u0006\u0007\u0008\u0009\u000a\u000b\u000c\u000d\u000e\u000f\u0010\u0011\u0012\u0013\u0014\u0015\u0016\u0017\u0018\u0019\u001a\u001b\u001c\u001d\u001e\u001f"`)
	want := `"\u0000\u0001\u0002\u0003\u0004\u0005\u0006\u0007\b\t\n\u000b\f\r\u000e\u000f\u0010\u0011\u0012\u0013\u0014\u0015\u0016\u0017\u0018\u0019\u001a\u001b\u001c\u001d\u001e\u001f"`
	got, err := Canonicalize(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("canonical controls = %q, want %q", got, want)
	}
}

func TestSHA256UsesSemanticCanonicalForm(t *testing.T) {
	left, err := SHA256([]byte(`{"b":1.0,"a":true}`))
	if err != nil {
		t.Fatalf("SHA256(left): %v", err)
	}
	right, err := SHA256([]byte(` { "a": true, "b": 10e-1 } `))
	if err != nil {
		t.Fatalf("SHA256(right): %v", err)
	}
	if left != right {
		t.Fatal("semantic equivalents produced different hashes")
	}
	different, err := SHA256([]byte(`{"a":false,"b":1}`))
	if err != nil {
		t.Fatalf("SHA256(different): %v", err)
	}
	if different == left {
		t.Fatal("different schemas produced the same hash")
	}
}

func loadConformanceFixture(t *testing.T) conformanceFixture {
	t.Helper()
	raw, err := os.ReadFile("../../../../contract-fixtures/execution/fused-canonical-json-v1.json")
	if err != nil {
		t.Fatalf("read conformance fixture: %v", err)
	}
	var fixture conformanceFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode conformance fixture: %v", err)
	}
	return fixture
}
