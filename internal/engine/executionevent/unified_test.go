package executionevent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestUnifiedPublisherPreservesZeroProviderAttempts checks the actual serialized envelope, not only the model.
func TestUnifiedPublisherPreservesZeroProviderAttempts(t *testing.T) {
	event := validUnifiedReceipt()
	stub := &publisherStub{}
	// Canonical normalization must never introduce a provider attempt onto a logical call.
	if err := NewPublisher(stub).Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	var envelope models.EngineExecutionEventEnvelope
	if err := json.Unmarshal(stub.message.Data, &envelope); err != nil {
		t.Fatal(err)
	}
	// The durable envelope carries hierarchy metadata independently of trace correlation.
	if envelope.Event.AttemptCount != 0 || envelope.Event.ExecutionKind != "unified" || len(envelope.Event.UnifiedSteps) != 1 {
		t.Fatal("logical envelope gained provider accounting")
	}
}

// TestUnifiedMetadataRejectsMalformedHierarchy prevents invalid durable documents from entering either producer or worker paths.
func TestUnifiedMetadataRejectsMalformedHierarchy(t *testing.T) {
	parent := validUnifiedReceipt()
	tests := []models.EngineExecutionEvent{parent, parent, parent, parent, parent}
	tests[0].ExecutionKind = "unknown"
	tests[1].AttemptCount = 1
	tests[2].ParentExecutionID = uuid.New()
	tests[3].UnifiedSteps = []models.UnifiedExecutionStep{{Target: "bad\nname", Phase: "forward", Status: "success"}}
	tests[4].UnifiedSteps = append(append([]models.UnifiedExecutionStep{}, parent.UnifiedSteps...), parent.UnifiedSteps...)
	for _, event := range tests {
		// Malformed metadata must fail before a provider receipt can be misclassified or orphaned.
		if ValidateUnifiedMetadata(event) == nil {
			t.Fatal("malformed hierarchy admitted")
		}
	}
}

// TestUnifiedChildCorrelationDoesNotRequireTracing preserves ordinary standalone receipts and server-owned child linkage.
func TestUnifiedChildCorrelationDoesNotRequireTracing(t *testing.T) {
	parent := uuid.New()
	event := models.EngineExecutionEvent{}
	AttachUnifiedChild(context.Background(), &event)
	// Ordinary executions do not acquire synthetic logical identity.
	if event.ParentExecutionID != uuid.Nil {
		t.Fatal("standalone receipt acquired parent")
	}
	ctx := WithUnifiedChild(context.Background(), parent, "items", "rollback")
	AttachUnifiedChild(ctx, &event)
	if event.ParentExecutionID != parent || event.UnifiedTarget != "items" || event.ExecutionPhase != "rollback" {
		t.Fatal("child correlation missing")
	}
	if err := ValidateUnifiedMetadata(event); err != nil {
		t.Fatal(err)
	}
}

// validUnifiedReceipt models a bounded, provider-free parent for shared contract tests.
func validUnifiedReceipt() models.EngineExecutionEvent {
	now := time.Now().UTC()
	return models.EngineExecutionEvent{ID: uuid.New(), ExecutionKind: "unified", Transport: "sdk", Status: "success", AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0", EndpointName: "items.read", StartedAt: now, EndedAt: now, UnifiedSteps: []models.UnifiedExecutionStep{{Target: "items", Phase: "forward", Status: "success"}}}
}
