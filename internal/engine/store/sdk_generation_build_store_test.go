package store

import (
	"strings"
	"testing"
)

// TestSDKGenerationBuildQueryDoesNotHideMissingPlan ensures corrupted recovery authority fails a worker pass visibly.
func TestSDKGenerationBuildQueryDoesNotHideMissingPlan(t *testing.T) {
	// An inner join would silently omit the pending app forever if applied-plan retention were damaged.
	if !strings.Contains(sdkGenerationBuildSelect, "LEFT JOIN LATERAL") {
		t.Fatalf("SDK generation recovery query can hide a missing applied plan: %s", sdkGenerationBuildSelect)
	}
}

// TestSDKGenerationTransitionFencesLatestAppliedPlan pins the Engine-side retry attempt boundary when Registry reuses a job ID.
func TestSDKGenerationTransitionFencesLatestAppliedPlan(t *testing.T) {
	// The CAS query must compare its plan ID with the newest applied non-noop plan, not merely any historical plan.
	for _, fragment := range []string{"AND $5 = (", "ORDER BY plan.applied_at DESC, plan.created_at DESC", "NOT COALESCE((plan.resolved_payload->>'noop')::boolean, false)"} {
		if !strings.Contains(sdkGenerationTransitionSQL, fragment) {
			t.Fatalf("SDK generation transition is missing attempt fence %q: %s", fragment, sdkGenerationTransitionSQL)
		}
	}
}
