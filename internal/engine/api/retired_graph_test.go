package api

import (
	"strings"
	"testing"
)

// TestRetiredGraphConfigRejected keeps obsolete graph authority out of Engine planning.
func TestRetiredGraphConfigRejected(t *testing.T) {
	var document sdkConfigDocument
	err := decodeAppConfigJSON([]byte(`{"unified_operations":{}}`), &document)
	// Unknown graph fields must fail decoding instead of becoming an empty app.
	if err == nil || !strings.Contains(err.Error(), "unified_operations") {
		t.Fatalf("retired graph authoring must fail explicitly: %v", err)
	}
}
