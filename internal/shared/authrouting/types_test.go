package authrouting

import "testing"

// TestEffectiveBasicPasswordModeDefaultsOnlyOmission proves provider exceptions stay explicit while standard Basic remains the default.
func TestEffectiveBasicPasswordModeDefaultsOnlyOmission(t *testing.T) {
	tests := []struct {
		name  string
		input BasicPasswordMode
		want  BasicPasswordMode
		valid bool
	}{
		{name: "omitted", want: BasicPasswordRequired, valid: true},
		{name: "required", input: BasicPasswordRequired, want: BasicPasswordRequired, valid: true},
		{name: "optional", input: BasicPasswordOptional, want: BasicPasswordOptional, valid: true},
		{name: "empty", input: BasicPasswordEmpty, want: BasicPasswordEmpty, valid: true},
		{name: "unknown", input: "unknown"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, valid := EffectiveBasicPasswordMode(test.input)
			// Both the effective value and validity flag are part of the shared fail-closed contract.
			if got != test.want || valid != test.valid {
				t.Fatalf("EffectiveBasicPasswordMode(%q) = (%q, %v), want (%q, %v)", test.input, got, valid, test.want, test.valid)
			}
		})
	}
}
