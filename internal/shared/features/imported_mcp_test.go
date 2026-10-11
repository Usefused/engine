package features

import "testing"

// TestImportedMCPOptIn proves missing, malformed, and false values cannot enable the preview.
func TestImportedMCPOptIn(t *testing.T) {
	for _, value := range []string{"", "false", "1", "TRUE", " true ", "true"} {
		t.Setenv(ImportedMCPEnvironment, value)
		// An exact opt-in avoids accidental activation from deployment defaults.
		if got := ImportedMCPEnabled(); got != (value == "true") {
			t.Fatalf("flag %q: enabled=%v", value, got)
		}
	}
}
