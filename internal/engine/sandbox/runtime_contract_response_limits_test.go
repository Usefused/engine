package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// TestRuntimeContractResponseBoundsEntireBatch verifies valid JSON and trailing bytes share one admission ceiling.
func TestRuntimeContractResponseBoundsEntireBatch(t *testing.T) {
	for _, payload := range []string{`{"data":{},"unknown":"` + strings.Repeat("x", 128) + `"}`, `{"data":{}}` + strings.Repeat(" ", 128)} {
		_, err := decodeBoundedRuntimeContracts(strings.NewReader(payload), 64)
		// A large ignored field or post-document padding must not bypass the aggregate byte limit.
		if !errors.Is(err, errRuntimeContractsResponseLimit) {
			t.Fatalf("limit result = %v", err)
		}
	}
}

// TestRuntimeContractResponseRejectsTrailingValues prevents partial JSON responses from being accepted as complete contracts.
func TestRuntimeContractResponseRejectsTrailingValues(t *testing.T) {
	for _, payload := range []string{`{"data":{}} {}`, `{"data":{}} invalid`} {
		_, err := decodeBoundedRuntimeContracts(strings.NewReader(payload), 1024)
		// The entire response must be exactly one object, not just an initially valid prefix.
		if err == nil {
			t.Fatal("accepted trailing content")
		}
	}
	_, err := decodeBoundedRuntimeContracts(strings.NewReader(`{"data":{"serviceRuntimeContracts":[]}}`), 1024)
	// Ordinary empty batches remain compatible with the existing wire representation.
	if err != nil {
		t.Fatal(err)
	}
}
