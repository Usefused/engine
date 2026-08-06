package models

import "testing"

func TestRuntimeEntitlementNormalizedEnablesDriftForLegacyRows(t *testing.T) {
	entitlement := (RuntimeEntitlement{DriftMonitoringEnabled: false}).Normalized()
	if !entitlement.DriftMonitoringEnabled {
		t.Fatal("legacy drift false value should normalize to enabled")
	}
}
