package models

import "testing"

func TestRuntimeEntitlementNormalizedEnablesDriftForLegacyRows(t *testing.T) {
	entitlement := (RuntimeEntitlement{DriftMonitoringEnabled: false}).Normalized()
	if !entitlement.DriftMonitoringEnabled {
		t.Fatal("legacy drift false value should normalize to enabled")
	}
}

// TestRuntimeEntitlementNormalizedDefaultsMissingLimits keeps older Registry bundles compatible with every Engine limit.
func TestRuntimeEntitlementNormalizedDefaultsMissingLimits(t *testing.T) {
	entitlement := (RuntimeEntitlement{}).Normalized()

	assertEntitlementLimit(t, "max buckets", entitlement.MaxBuckets, -1)
	assertEntitlementLimit(t, "max API families", entitlement.MaxAPIFamilies, -1)
	assertEntitlementLimit(t, "max SDK families", entitlement.MaxSDKFamilies, -1)
	assertEntitlementLimit(t, "max MCP families", entitlement.MaxMCPFamilies, -1)
	assertEntitlementLimit(t, "max services", entitlement.MaxServices, -1)
	assertEntitlementLimit(t, "max sandbox concurrency", entitlement.MaxSandboxConcurrency, -1)
	assertEntitlementLimit(t, "execution retention days", entitlement.ExecutionRetentionDays, 30)
}

// TestRuntimeEntitlementNormalizedPreservesExplicitZeroLimits proves an explicit deny survives normalization for every capability.
func TestRuntimeEntitlementNormalizedPreservesExplicitZeroLimits(t *testing.T) {
	zero := 0
	entitlement := (RuntimeEntitlement{
		MaxBuckets:             &zero,
		MaxAPIFamilies:         &zero,
		MaxSDKFamilies:         &zero,
		MaxMCPFamilies:         &zero,
		MaxServices:            &zero,
		MaxSandboxConcurrency:  &zero,
		ExecutionRetentionDays: &zero,
	}).Normalized()

	assertEntitlementLimit(t, "max buckets", entitlement.MaxBuckets, 0)
	assertEntitlementLimit(t, "max API families", entitlement.MaxAPIFamilies, 0)
	assertEntitlementLimit(t, "max SDK families", entitlement.MaxSDKFamilies, 0)
	assertEntitlementLimit(t, "max MCP families", entitlement.MaxMCPFamilies, 0)
	assertEntitlementLimit(t, "max services", entitlement.MaxServices, 0)
	assertEntitlementLimit(t, "max sandbox concurrency", entitlement.MaxSandboxConcurrency, 0)
	assertEntitlementLimit(t, "execution retention days", entitlement.ExecutionRetentionDays, 0)
}

func assertEntitlementLimit(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}
