package sandbox

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

func TestRuntimePaginationV3ValidatesEffectiveRequestTypeAtSnapshotBoundary(t *testing.T) {
	initial := ""
	policy := &paginationpolicy.Config{
		Version: paginationpolicy.Version,
		Request: []paginationpolicy.RequestStep{{
			State: "cursor", Target: paginationpolicy.RequestTarget{Location: "query", Name: "cursor"},
			ValueType: "string", Initial: &paginationpolicy.Scalar{Type: "string", String: &initial}, Apply: "all",
		}},
		Response: paginationpolicy.ResponsePlan{
			Items:  paginationpolicy.ItemsSource{Path: "$.items"},
			Values: []paginationpolicy.ResponseValue{{Name: "next", Source: paginationpolicy.ValueSource{Location: "body", Path: "$.next", ValueType: "string"}}},
		},
		Continuation: []paginationpolicy.ContinuationStep{{Kind: "token", State: "cursor", ResponseValue: "next"}},
		Termination:  paginationpolicy.Termination{StopOnMissingValues: []string{"next"}, RepeatedValue: "error"},
		Limits:       paginationpolicy.Limits{MaxPages: 10, MaxItems: 100, MaxBytes: 1 << 20, MaxDurationMs: 5_000},
	}
	service := &runtimeContractService{Pagination: policy}
	endpoint := fusedobject.Endpoint{Parameters: fusedobject.Parameters{{Name: "cursor", In: "query", Type: "integer"}}}

	err := validateRuntimePagination(service, []fusedobject.Endpoint{endpoint})
	if err == nil || !strings.Contains(err.Error(), "request_target_invalid") {
		t.Fatalf("snapshot target validation error = %v", err)
	}

	endpoint.Parameters[0].Type = "string"
	if err := validateRuntimePagination(service, []fusedobject.Endpoint{endpoint}); err != nil {
		t.Fatalf("valid effective service pagination was rejected: %v", err)
	}
}
