package worker

import (
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestExecutionWorkerAcceptsPreviousEnvelope supports draining pre-upgrade physical receipts on the new worker.
func TestExecutionWorkerAcceptsPreviousEnvelope(t *testing.T) {
	for _, version := range []int{5, models.EngineExecutionEventSchemaVersion} {
		data, err := json.Marshal(models.EngineExecutionEventEnvelope{SchemaVersion: version, Event: validExecutionEvent()})
		// Fixture serialization must succeed before exercising compatibility admission.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeExecutionMessage(data); err != nil {
			t.Fatalf("supported envelope %d rejected: %v", version, err)
		}
	}
}

// TestExecutionWorkerRejectsPoisonHierarchy avoids an endlessly retried database batch after malformed publication.
func TestExecutionWorkerRejectsPoisonHierarchy(t *testing.T) {
	event := validExecutionEvent()
	event.ExecutionKind = "unrecognized"
	message := executionMessage(t, event)
	// Malformed kind metadata is rejected by worker admission, before the store owns a batch.
	if _, err := decodeExecutionMessage(message.Data); err == nil {
		t.Fatal("poison hierarchy reached persistence")
	}
}
