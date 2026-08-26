package store

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestUnifiedReceiptFilteringKeepsAccountingSeparate locks root/child selection to SQL scope.
func TestUnifiedReceiptFilteringKeepsAccountingSeparate(t *testing.T) {
	accountID, familyID, parentID := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name   string
		filter EngineExecutionFilter
		suffix string
		args   []any
	}{
		{"aggregate", EngineExecutionFilter{AccountID: accountID, AppFamilyID: familyID}, "AND execution_kind = 'physical'", []any{accountID, familyID}},
		{"roots", EngineExecutionFilter{AccountID: accountID, AppFamilyID: familyID, ReceiptRoots: true}, "AND parent.execution_kind = 'unified'))", []any{accountID, familyID}},
		{"children", EngineExecutionFilter{AccountID: accountID, AppFamilyID: familyID, ReceiptRoots: true, ParentExecutionID: parentID}, "AND execution_kind = 'physical' AND parent_execution_id = $3", []any{accountID, familyID, parentID}},
		{"unscoped parent", EngineExecutionFilter{AccountID: accountID, ParentExecutionID: parentID}, "AND execution_kind = 'physical'", []any{accountID}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			where, args := engineExecutionWhereClause(test.filter)
			// Root grouping and provider accounting must remain mutually exclusive query modes.
			if !strings.HasSuffix(where, test.suffix) || !reflect.DeepEqual(args, test.args) {
				t.Fatalf("query scope = %s / %v", where, args)
			}
		})
	}
}

// TestUnifiedOrphanChildRemainsVisible preserves physical audit evidence during
// out-of-order delivery, failed parent publication, and differential retention.
func TestUnifiedOrphanChildRemainsVisible(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	parent, child := unifiedReceiptStoreEvents(fixture.event)
	// Model child-first JetStream delivery with no parent present yet.
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, []models.EngineExecutionEvent{child}); err != nil {
		t.Fatal(err)
	}
	filter := EngineExecutionFilter{AccountID: child.AccountID, AppFamilyID: child.AppFamilyID, AppID: child.AppID, Limit: 50}
	rows, total, err := fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	visible := requireSingleExecutionEvent(t, rows, total, err)
	// Missing parent evidence must not hide a successfully persisted physical receipt.
	if visible.ID != child.ID {
		t.Fatal("orphan child was hidden")
	}
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, []models.EngineExecutionEvent{parent}); err != nil {
		t.Fatal(err)
	}
	rows, total, err = fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	visible = requireSingleExecutionEvent(t, rows, total, err)
	// Arrival of the logical envelope groups the child without a duplicate top-level row.
	if visible.ID != parent.ID {
		t.Fatal("parent arrival did not group child")
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `DELETE FROM fused_engine_execution_events WHERE id=$1`, parent.ID); err != nil {
		t.Fatal(err)
	}
	rows, total, err = fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	visible = requireSingleExecutionEvent(t, rows, total, err)
	// Parent expiration cannot make a later-starting child disappear before its own retention period.
	if visible.ID != child.ID {
		t.Fatal("retention hid surviving child")
	}
}

// TestUnifiedReceiptPersistenceAndAccounting verifies real SQL parent grouping,
// physical analytics, bounded child loading, and cross-account isolation.
func TestUnifiedReceiptPersistenceAndAccounting(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	parent, child := unifiedReceiptStoreEvents(fixture.event)
	// A replayed envelope must not create duplicate parents or physical usage rows.
	if err := fixture.repository.BatchCreateEngineExecutionEvents(fixture.ctx, []models.EngineExecutionEvent{parent, child, parent}); err != nil {
		t.Fatal(err)
	}
	filter := EngineExecutionFilter{AccountID: child.AccountID, AppFamilyID: child.AppFamilyID, AppID: child.AppID, Limit: 50}
	rows, total, err := fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	root := requireSingleExecutionEvent(t, rows, total, err)
	// Logical metadata is retained even though no service UUID exists on a parent.
	if root.ID != parent.ID || !reflect.DeepEqual(root.UnifiedSteps, parent.UnifiedSteps) {
		t.Fatal("logical root did not round-trip")
	}
	filter.ParentExecutionID = parent.ID
	rows, total, err = fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	storedChild := requireSingleExecutionEvent(t, rows, total, err)
	// Child navigation must retain ordinary provider timing and phase.
	if storedChild.ID != child.ID || storedChild.ExecutionPhase != "forward" || storedChild.ProviderLatencyMs == nil {
		t.Fatal("child metadata did not round-trip")
	}
	assertUnifiedProviderAnalytics(t, fixture, child)
	filter.AccountID = uuid.New()
	_, total, err = fixture.repository.ListEngineExecutionEventsByApp(fixture.ctx, filter)
	// A known parent UUID is never a capability to read another account's receipts.
	if err != nil || total != 0 {
		t.Fatal("cross-account parent lookup was not empty")
	}
}

// unifiedReceiptStoreEvents creates metadata-only synthetic receipts without a live app or provider.
func unifiedReceiptStoreEvents(child models.EngineExecutionEvent) (models.EngineExecutionEvent, models.EngineExecutionEvent) {
	parent := models.EngineExecutionEvent{ID: uuid.New(), ExecutionKind: "unified", AccountID: child.AccountID, AppFamilyID: child.AppFamilyID, AppID: child.AppID,
		AppVersion: child.AppVersion, Transport: child.Transport, Direction: child.Direction, EndpointName: "items.read", Status: "success",
		StartedAt: child.StartedAt, EndedAt: child.EndedAt, CreatedAt: child.CreatedAt, LatencyMs: 100,
		UnifiedSteps: []models.UnifiedExecutionStep{{Target: "source", Phase: "forward", Status: "success"}}}
	child.ParentExecutionID, child.UnifiedTarget, child.ExecutionPhase = parent.ID, "source", "forward"
	latency := int64(7)
	child.ProviderLatencyMs = &latency
	return parent, child
}

// assertUnifiedProviderAnalytics checks each public accounting view rather than relying on the app list count.
func assertUnifiedProviderAnalytics(t *testing.T, fixture executionActivityFixture, child models.EngineExecutionEvent) {
	t.Helper()
	filter := EngineExecutionFilter{AccountID: child.AccountID, AppFamilyID: child.AppFamilyID, AppID: child.AppID, Limit: 50}
	app, err := fixture.repository.GetEngineExecutionAnalyticsByApp(fixture.ctx, filter)
	// The logical parent must not add to app totals or inflate average provider execution duration.
	if err != nil || app.TotalCalls != 1 {
		t.Fatalf("app provider totals = %#v, %v", app, err)
	}
	workspace, err := fixture.repository.GetWorkspaceExecutionAnalytics(fixture.ctx, child.AccountID, child.StartedAt.Add(-time.Minute), child.StartedAt.Add(time.Minute))
	// Workspace provider totals use the same exclusion even though their SQL is independent.
	if err != nil || workspace.TotalCalls != 1 {
		t.Fatalf("workspace provider totals = %#v, %v", workspace, err)
	}
	mcp, err := fixture.repository.GetMCPAnalyticsDashboard(fixture.ctx, child.AppID)
	// Session dashboards and tool breakdowns remain provider-level analytics.
	if err != nil || mcp.TotalRequests != 1 {
		t.Fatalf("MCP provider totals = %#v, %v", mcp, err)
	}
	filter.ServiceID, filter.AppFamilyID, filter.AppID = child.ServiceID, uuid.Nil, uuid.Nil
	rows, total, err := fixture.repository.ListEngineExecutionEventsByService(fixture.ctx, filter)
	stored := requireSingleExecutionEvent(t, rows, total, err)
	// Service Activity always displays individual requests, including Unified children.
	if stored.ID != child.ID {
		t.Fatal("service activity did not retain physical child")
	}
}
