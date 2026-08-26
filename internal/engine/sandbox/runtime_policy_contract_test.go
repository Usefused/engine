package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
)

func TestRuntimePolicyContractsRejectNonV3WithCompatibilityClassification(t *testing.T) {
	tests := map[string]func() error{
		"rate limit": func() error {
			snapshot := &store.ServiceContractSnapshot{ServiceMetadata: fusedobject.ServiceMetadata{
				RateLimit: &ratelimitpolicy.Config{Version: 2},
			}}
			return validateRuntimeSnapshot(snapshot)
		},
		"retry": func() error {
			snapshot := &store.ServiceContractSnapshot{ServiceMetadata: fusedobject.ServiceMetadata{
				RetryConfig: &retrypolicy.Config{Version: 2},
			}}
			return validateRuntimeSnapshot(snapshot)
		},
		"pagination": func() error {
			return validateRuntimePaginationConfig("service", &paginationpolicy.Config{Version: 2})
		},
	}
	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			assertPolicyCompatibilityError(t, run())
		})
	}
}

// TestRuntimeContractDecodeRejectsRemovedPolicyFieldsAsIncompatible mirrors a
// nonempty production request while rejecting removed fields before admission.
func TestRuntimeContractDecodeRejectsRemovedPolicyFieldsAsIncompatible(t *testing.T) {
	payload := `{"data":{"serviceRuntimeContracts":[{"contract_version":2,"required_capabilities":[],"service":{"rate_limit":{"version":3,"policies":[],"retry_after":{"enabled":true,"max_delay_ms":1}}}}]}}`
	_, version := recoveryContractFixture()
	_, err := decodeRuntimeContractsResponse(strings.NewReader(payload), []store.WorkspaceServiceVersion{version})
	assertPolicyCompatibilityError(t, err)
}

func assertPolicyCompatibilityError(t *testing.T, err error) {
	t.Helper()
	var compatibility *fusedobject.ExecutionContractCompatibilityError
	if !errors.As(err, &compatibility) || compatibility.Reason != fusedobject.ExecutionContractReasonUnsupportedCapability {
		t.Fatalf("policy compatibility error = %v", err)
	}
}
