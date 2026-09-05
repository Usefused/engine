package sandbox

import (
	"errors"
	"fmt"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"testing"
)

// TestRuntimeContractRejectionVersionRejectsProseLookalikes preserves typed source-repair identity through wrapping.
func TestRuntimeContractRejectionVersionRejectsProseLookalikes(t *testing.T) {
	serviceID, versionID := uuid.New(), uuid.New()
	rejection := runtimeContractGraphQLError{}
	rejection.Message = "database password=fsk_never_return"
	rejection.Extensions.Code = "runtime_contract_rejected"
	rejection.Extensions.ServiceVersionID = versionID
	err := classifyRuntimeContractGraphQLErrors([]runtimeContractGraphQLError{rejection}, []store.WorkspaceServiceVersion{{ServiceID: serviceID, ServiceVersionID: versionID}})
	got, ok := RuntimeContractRejectionVersion(fmt.Errorf("fetch: %w", err))
	// Exact version identity survives wrappers without exporting source payloads.
	if !ok || got != versionID {
		t.Fatalf("typed rejection: %s %v", got, ok)
	}
	_, reason, ok := RuntimeContractRejectionDetails(err)
	// Missing explicit detail uses reviewed copy and never falls back to the top-level GraphQL diagnostic.
	if !ok || reason != defaultRuntimeContractRejectionReason {
		t.Fatalf("fallback reason=%q ok=%t", reason, ok)
	}
	_, ok = RuntimeContractRejectionVersion(errors.New("runtime_contract_rejected"))
	// Error prose is not authority to turn outages into permanent rejections.
	if ok {
		t.Fatal("accepted prose lookalike")
	}
}
