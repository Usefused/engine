package models

import "testing"

func TestRuntimeEntitlementNormalizedEnablesDriftForLegacyRows(t *testing.T) {
	entitlement := (RuntimeEntitlement{DriftMonitoringEnabled: false}).Normalized()
	if !entitlement.DriftMonitoringEnabled {
		t.Fatal("legacy drift false value should normalize to enabled")
	}
}

func TestRuntimeEntitlementNormalizedDefaultsMissingLimits(t *testing.T) {
	entitlement := (RuntimeEntitlement{}).Normalized()

	assertEntitlementLimit(t, "max buckets", entitlement.MaxBuckets, -1)
	assertEntitlementLimit(t, "max SDK families", entitlement.MaxSDKFamilies, -1)
	assertEntitlementLimit(t, "max MCP families", entitlement.MaxMCPFamilies, -1)
	assertEntitlementLimit(t, "max services", entitlement.MaxServices, -1)
	assertEntitlementLimit(t, "max sandbox concurrency", entitlement.MaxSandboxConcurrency, -1)
	assertEntitlementLimit(t, "execution retention days", entitlement.ExecutionRetentionDays, 30)
}

func TestRuntimeEntitlementNormalizedPreservesExplicitZeroLimits(t *testing.T) {
	zero := 0
	entitlement := (RuntimeEntitlement{
		MaxBuckets:             &zero,
		MaxSDKFamilies:         &zero,
		MaxMCPFamilies:         &zero,
		MaxServices:            &zero,
		MaxSandboxConcurrency:  &zero,
		ExecutionRetentionDays: &zero,
	}).Normalized()

	assertEntitlementLimit(t, "max buckets", entitlement.MaxBuckets, 0)
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
