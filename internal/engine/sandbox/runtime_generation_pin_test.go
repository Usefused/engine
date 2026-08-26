package sandbox

import (
	"github.com/Usefused/engine/internal/shared/models"
	"strings"
	"testing"
)

// TestRuntimeSnapshotRetainsRegistryGenerationIdentity verifies activation transfers immutable generation identity without substituting the runtime hash.
func TestRuntimeSnapshotRetainsRegistryGenerationIdentity(t *testing.T) {
	item, requested := recoveryContractFixture()
	item.Revision, item.SourceHash = 9, "source-9"
	item.GenerationContractHash = "sha256:" + strings.Repeat("c", 64)
	item.Service.Provider = &models.ServiceProviderIdentity{Name: "Fixture provider", Handle: "fixture-provider"}
	snapshot, err := runtimeContractSnapshotFromBatchItem(item, requested)
	// The fixture must cross the normal runtime validation boundary before persistence.
	if err != nil {
		t.Fatal(err)
	}
	// Snapshot identity is copied exactly from Registry, not recalculated from the lean runtime projection.
	if snapshot.Revision != 9 || snapshot.SourceHash != item.SourceHash || snapshot.GenerationContractHash != item.GenerationContractHash {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	// Provider identity must survive the same projection so qualified local references remain repairable by refresh.
	if snapshot.ServiceMetadata.Provider == nil || snapshot.ServiceMetadata.Provider.Handle != "fixture-provider" {
		t.Fatal("provider identity lost")
	}
	for _, field := range []string{"revision", "source_hash", "generation_contract_hash"} {
		// Production GraphQL must request the same fields that the decoder retains.
		if !strings.Contains(runtimeContractsQuery, field) {
			t.Fatalf("query omits %s", field)
		}
	}
}
