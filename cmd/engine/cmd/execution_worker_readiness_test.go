package cmd

import (
	"context"
	"testing"
)

// TestExecutionWorkerReadinessGatesHostedEngine verifies truthful capability reporting without disabling raw-only hosts.
func TestExecutionWorkerReadinessGatesHostedEngine(t *testing.T) {
	called := false
	// A deterministic failed probe keeps the test independent of local namespace policy.
	checker := func(context.Context) bool {
		called = true
		return false
	}
	t.Setenv("FUSED_UNIFIED_APP_WORKER_REQUIRED", "")
	// Optional workers must still be probed; reporting them as available would hide unsupported hosts.
	if probeExecutionWorkerReadiness(checker) || !called || executionWorkerBlocksReadiness(false) {
		t.Fatal("optional worker availability was misreported")
	}
	t.Setenv("FUSED_UNIFIED_APP_WORKER_REQUIRED", "true")
	// A failed real-worker probe must fail hosted readiness before app traffic is served.
	if probeExecutionWorkerReadiness(checker) || !executionWorkerBlocksReadiness(false) || executionWorkerBlocksReadiness(true) {
		t.Fatal("unavailable hosted worker did not fail readiness")
	}
}
