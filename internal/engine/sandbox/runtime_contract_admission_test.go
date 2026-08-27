package sandbox

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// TestValidateRuntimeContractSnapshotRunsTransportAndStoreAdmission proves preflight uses the complete existing Engine boundary.
func TestValidateRuntimeContractSnapshotRunsTransportAndStoreAdmission(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	valid := store.ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 serviceID,
		ServiceVersionID:          serviceVersionID,
		Version:                   "2026-08-27",
		ServiceMetadata:           fusedobject.ServiceMetadata{ID: serviceID, ServiceVersionID: serviceVersionID},
	}
	// The minimal identity-only snapshot must pass every layer before its hash is inspected.
	if err := ValidateRuntimeContractSnapshot(&valid); err != nil {
		t.Fatalf("ValidateRuntimeContractSnapshot(valid): %v", err)
	}
	// A non-empty hash proves store schema and canonical hash admission ran after transport checks.
	if valid.ContractHash == "" {
		t.Fatal("validated runtime contract did not receive a contract hash")
	}

	invalidTransport := valid
	invalidTransport.Endpoints = []fusedobject.Endpoint{{ID: uuid.New(), Name: "missingProtocol", Method: "GET", Path: "/items"}}
	// Missing provider protocol is an Engine transport defect, not a persistence identity defect.
	if err := ValidateRuntimeContractSnapshot(&invalidTransport); err == nil || !strings.Contains(err.Error(), "provider protocol") {
		t.Fatalf("transport admission error = %v", err)
	}

	invalidMetadataIdentity := valid
	invalidMetadataIdentity.ServiceMetadata.ID = uuid.New()
	// Nested metadata cannot substitute a different service identity during preflight mapping.
	if err := ValidateRuntimeContractSnapshot(&invalidMetadataIdentity); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("metadata identity admission error = %v", err)
	}

	invalidStoreState := valid
	invalidStoreState.Status = "deleted"
	// Snapshot lifecycle state is checked by the same store validator used immediately before writes.
	if err := ValidateRuntimeContractSnapshot(&invalidStoreState); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("store admission error = %v", err)
	}
}
