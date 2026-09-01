package store

import (
	"strings"
	"testing"
)

// TestListServiceConsumersKeepsBuildingDependencies pins the removal guard for SDKs waiting on package generation.
func TestListServiceConsumersKeepsBuildingDependencies(t *testing.T) {
	// All lifecycle states that may still require the selected service must remain in the dependency projection.
	if !strings.Contains(listServiceConsumersSQL, "app.status IN ('building', 'active', 'deprecated')") {
		t.Fatalf("service consumer query does not retain building dependencies: %s", listServiceConsumersSQL)
	}
}
