package unified

import (
	"strings"
	"testing"
)

// TestValidPublicNameKeepsExecutionAndAuditAdmissionAligned prevents executable targets from losing receipts.
func TestValidPublicNameKeepsExecutionAndAuditAdmissionAligned(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"read_items", true}, {"@example/source", true}, {"réad", true}, {"read\nitems", false}, {"read\titems", false}, {"\xff", false}, {" read", false}, {"", false}, {strings.Repeat("a", 254), false},
	} {
		// Exact bounded names may be Unicode, but never invalid text or control characters.
		if got := ValidPublicName(test.value, 253); got != test.valid {
			t.Errorf("public name admission mismatch for %q", test.value)
		}
	}
}
