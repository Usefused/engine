package canonicaljson

import (
	"strings"
	"testing"
)

type conformanceVector struct {
	Name      string
	Inputs    []string
	Canonical string
	SHA256    string
}

type conformanceFixture struct {
	Version               string `json:"version"`
	MaxInputBytes         int    `json:"max_input_bytes"`
	MaxDepth              int    `json:"max_depth"`
	MaxValues             int    `json:"max_values"`
	MaxNumberDigits       int    `json:"max_number_digits"`
	MaxAbsDecimalExponent int    `json:"max_abs_decimal_exponent"`
	Vectors               []conformanceVector
	InvalidInputs         []string `json:"invalid_inputs"`
}

// TestCanonicalizeConformsToFusedV1Fixture keeps Engine's byte contract exact
// using local vectors while the integration workspace compares all runtimes.
func TestCanonicalizeConformsToFusedV1Fixture(t *testing.T) {
	fixture := localConformanceFixture()
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

// localConformanceFixture selects high-value ordering, numeric, and Unicode
// vectors without importing a sibling repository's complete golden corpus.
func localConformanceFixture() conformanceFixture {
	return conformanceFixture{
		Version: Version, MaxInputBytes: MaxInputBytes, MaxDepth: MaxDepth,
		MaxValues: MaxValues, MaxNumberDigits: MaxNumberDigits,
		MaxAbsDecimalExponent: MaxAbsDecimalExponent,
		Vectors: []conformanceVector{
			{Name: "object_order", Inputs: []string{`{"b":1,"a":true}`, ` { "a": true, "b": 1.0 } `}, Canonical: `{"a":true,"b":1}`, SHA256: "a3f44886ecd0b8667b0c6a4652d41e1f9e8205fb8d35d299fd20577f5268adb6"},
			{Name: "arbitrary_precision", Inputs: []string{"9007199254740993", "9.007199254740993e15"}, Canonical: "9.007199254740993e15", SHA256: "7b84848db20f8bd9b3c65bb3b641f22cf322702275ac0bb075169ac3740852fe"},
			{Name: "unicode_order", Inputs: []string{`{"é":"café","a":"😀"}`, `{"a":"\ud83d\ude00","é":"caf\u00e9"}`}, Canonical: `{"a":"😀","é":"café"}`, SHA256: "e9f7ad29af0306464d2c5a396e3d34764335f05b7cdd2cc1a255fd9399e78f6c"},
		},
		InvalidInputs: []string{"", `{"a":1} trailing`, `{"a":1,"a":2}`, "1e16384"},
	}
}
