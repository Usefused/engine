package cmd

import (
	"context"
	"testing"
)

// TestExecutionWorkerReadinessGatesHostedEngine verifies only opted-in hosted Engines require isolation.
func TestExecutionWorkerReadinessGatesHostedEngine(t *testing.T) {
	called := false
	// A deterministic failed probe keeps the test independent of local namespace policy.
	checker := func(context.Context) bool {
		called = true
		return false
	}
	t.Setenv("FUSED_EXECUTION_APP_WORKER_REQUIRED", "")
	// Raw-only local Engines should not depend on the hosted worker binary.
	if !probeExecutionWorkerReadiness(checker) || called {
		t.Fatal("optional worker unexpectedly gated Engine")
	}
	t.Setenv("FUSED_EXECUTION_APP_WORKER_REQUIRED", "true")
	// A failed real-worker probe must fail hosted readiness before app traffic is served.
	if probeExecutionWorkerReadiness(checker) || !called {
		t.Fatal("unavailable hosted worker did not fail readiness")
	}
}
