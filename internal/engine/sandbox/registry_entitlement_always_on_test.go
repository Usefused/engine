package sandbox

import "testing"

// TestRuntimeEntitlementFromHandshakeAlwaysOn keeps the Registry plan gate intact at the Engine boundary.
func TestRuntimeEntitlementFromHandshakeAlwaysOn(t *testing.T) {
	// Missing and Dev contracts must leave persistent app workers unavailable.
	if RuntimeEntitlementFromHandshake(nil).UnifiedAppAlwaysOnEnabled {
		t.Fatal("missing Registry entitlement enabled always-on Unified Apps")
	}
	if RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev"}).UnifiedAppAlwaysOnEnabled {
		t.Fatal("Dev entitlement enabled always-on Unified Apps")
	}
	// A missing wire field comes from an older Registry, so retain the prior worker capacity.
	if got := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev"}); got.MaxUnifiedAppConcurrency == nil || *got.MaxUnifiedAppConcurrency != 4 {
		t.Fatalf("older Registry must preserve four worker slots: %#v", got.MaxUnifiedAppConcurrency)
	}
	// Scale-up eligibility is independent of provider-call concurrency.
	maxUnifiedApps := 5
	got := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "scale-up", UnifiedAppAlwaysOnEnabled: true, MaxUnifiedAppFamilies: &maxUnifiedApps})
	if !got.UnifiedAppAlwaysOnEnabled {
		t.Fatal("Scale-up entitlement lost always-on Unified App eligibility")
	}
	// The independent family ceiling must survive the same wire conversion.
	if got.MaxUnifiedAppFamilies == nil || *got.MaxUnifiedAppFamilies != 5 {
		t.Fatalf("Scale-up Unified App family ceiling was lost: %#v", got.MaxUnifiedAppFamilies)
	}
	// A plan's explicit limit must survive conversion independently of provider concurrency.
	two := 2
	limited := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev", MaxUnifiedAppConcurrency: &two})
	// The configured Dev ceiling has to reach the runtime gate unchanged.
	if limited.MaxUnifiedAppConcurrency == nil || *limited.MaxUnifiedAppConcurrency != 2 {
		t.Fatalf("Unified App concurrency limit was lost: %#v", limited.MaxUnifiedAppConcurrency)
	}
	zero := 0
	blocked := RuntimeEntitlementFromHandshake(&rawRuntimeEntitlement{Plan: "dev", MaxUnifiedAppConcurrency: &zero})
	// Explicit zero is a plan denial rather than a missing-field compatibility case.
	if blocked.MaxUnifiedAppConcurrency == nil || *blocked.MaxUnifiedAppConcurrency != 0 {
		t.Fatalf("Unified App concurrency zero was lost: %#v", blocked.MaxUnifiedAppConcurrency)
	}
}
