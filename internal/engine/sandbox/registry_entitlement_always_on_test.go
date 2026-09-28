package sandbox

import "testing"

// TestRuntimeEntitlementFromHandshakeAlwaysOn keeps the Registry plan gate intact at the Engine boundary.
func TestRuntimeEntitlementFromHandshakeAlwaysOn(t *testing.T) {
	// Missing and Dev contracts must leave persistent app workers unavailable.
	if RuntimeEntitlementFromHandshake(nil).ExecutionAppAlwaysOnEnabled {
		t.Fatal("missing Registry entitlement enabled always-on Execution Apps")
	}
	if RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev"}).ExecutionAppAlwaysOnEnabled {
		t.Fatal("Dev entitlement enabled always-on Execution Apps")
	}
	// A missing wire field comes from an older Registry, so retain the prior worker capacity.
	if got := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev"}); got.MaxExecutionAppConcurrency == nil || *got.MaxExecutionAppConcurrency != 4 {
		t.Fatalf("older Registry must preserve four worker slots: %#v", got.MaxExecutionAppConcurrency)
	}
	// Scale-up eligibility is independent of provider-call concurrency.
	maxExecutionApps := 5
	got := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "scale-up", ExecutionAppAlwaysOnEnabled: true, MaxExecutionAppFamilies: &maxExecutionApps})
	if !got.ExecutionAppAlwaysOnEnabled {
		t.Fatal("Scale-up entitlement lost always-on Execution App eligibility")
	}
	// The independent family ceiling must survive the same wire conversion.
	if got.MaxExecutionAppFamilies == nil || *got.MaxExecutionAppFamilies != 5 {
		t.Fatalf("Scale-up Execution App family ceiling was lost: %#v", got.MaxExecutionAppFamilies)
	}
	// A plan's explicit limit must survive conversion independently of provider concurrency.
	two := 2
	limited := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev", MaxExecutionAppConcurrency: &two})
	// The configured Dev ceiling has to reach the runtime gate unchanged.
	if limited.MaxExecutionAppConcurrency == nil || *limited.MaxExecutionAppConcurrency != 2 {
		t.Fatalf("Execution App concurrency limit was lost: %#v", limited.MaxExecutionAppConcurrency)
	}
	zero := 0
	blocked := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev", MaxExecutionAppConcurrency: &zero})
	// Explicit zero is a plan denial rather than a missing-field compatibility case.
	if blocked.MaxExecutionAppConcurrency == nil || *blocked.MaxExecutionAppConcurrency != 0 {
		t.Fatalf("Execution App concurrency zero was lost: %#v", blocked.MaxExecutionAppConcurrency)
	}
}
