package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/executionevent"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// mcpConsumptionRuntimeStore extends the modern runtime fixture to observe MCP webhook consumption writes.
type mcpConsumptionRuntimeStore struct {
	*mcpModernRuntimeStore
	mu      sync.Mutex
	records []models.WebhookConsumption
}

// RecordWebhookConsumption stores each MCP consumption event for test verification.
func (s *mcpConsumptionRuntimeStore) RecordWebhookConsumption(_ context.Context, record models.WebhookConsumption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

// snapshot copies recorded MCP consumption entries safely for assertions.
func (s *mcpConsumptionRuntimeStore) snapshot() []models.WebhookConsumption {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.WebhookConsumption, len(s.records))
	copy(out, s.records)
	return out
}

// TestMCPModernResourceReadRecordsWebhookConsumption verifies reading a retained event records MCP consumer metadata and skips empty reads.
func TestMCPModernResourceReadRecordsWebhookConsumption(t *testing.T) {
	fixture := installMCPModernFixture(t)
	baseStore := globalMCPEventRuntimeStore.(*mcpModernRuntimeStore)
	recorder := &mcpConsumptionRuntimeStore{mcpModernRuntimeStore: baseStore}
	globalMCPEventRuntimeStore = recorder

	// Reading before any webhook arrives returns latest=null and must not fabricate a consumption record.
	emptyReq := newMCPModernRequest(t, fixture, "read-empty", "resources/read", map[string]any{"uri": fixture.uri})
	emptyRec := httptest.NewRecorder()
	fixture.router.ServeHTTP(emptyRec, emptyReq)
	// Confirm the empty read succeeded cleanly without recording consumption.
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("empty resources/read status=%d body=%s", emptyRec.Code, emptyRec.Body.String())
	}
	if len(recorder.snapshot()) != 0 {
		t.Fatalf("expected zero consumptions for empty resource read, got %d", len(recorder.snapshot()))
	}

	// Publish one retained webhook occurrence with a stable message ID, then read it through MCP.
	msgID := uuid.NewString()
	msg := nats.NewMsg(fixture.subject)
	msg.Header.Set("X-Webhook-Msg-ID", msgID)
	msg.Data = []byte(`{"id":"evt_1","amount":2500}`)
	if _, err := fixture.natsClient.JS.PublishMsg(msg); err != nil {
		t.Fatalf("publish webhook message: %v", err)
	}

	readReq := newMCPModernRequest(t, fixture, "read-live", "resources/read", map[string]any{"uri": fixture.uri})
	readRec := httptest.NewRecorder()
	fixture.router.ServeHTTP(readRec, readReq)
	// A successful snapshot read must record MCP consumption for the exact message UUID.
	if readRec.Code != http.StatusOK {
		t.Fatalf("resources/read status=%d body=%s", readRec.Code, readRec.Body.String())
	}
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("expected 1 MCP consumption record, got %d", len(records))
	}
	if records[0].EventID != executionevent.WebhookEventID(msgID) {
		t.Fatalf("event_id=%s want=%s", records[0].EventID, executionevent.WebhookEventID(msgID))
	}
	if records[0].ConsumerKind != "mcp" || records[0].AccountID != fixture.accountID {
		t.Fatalf("unexpected consumer record: %+v", records[0])
	}
	if records[0].AppID == nil || *records[0].AppID != fixture.appID || records[0].AppFamilyID == nil || *records[0].AppFamilyID != fixture.familyID {
		t.Fatalf("unexpected app identity on MCP consumption: %+v", records[0])
	}
	if records[0].ConsumedAt.IsZero() {
		t.Fatal("expected non-zero consumed_at timestamp")
	}

	// Legacy sequence-derived IDs and blank IDs must be ignored without creating synthetic receipts.
	recordMCPWebhookConsumption(context.Background(), auth.RuntimeIdentity{AccountID: fixture.accountID, AppID: fixture.appID}, "nats:42")
	recordMCPWebhookConsumption(context.Background(), auth.RuntimeIdentity{AccountID: fixture.accountID, AppID: fixture.appID}, "")
	if len(recorder.snapshot()) != 1 {
		t.Fatalf("expected legacy/blank IDs to be skipped, got %d records", len(recorder.snapshot()))
	}
}
