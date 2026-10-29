package store

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestServiceLifecyclePurgePreservesExecutionNames proves deletion releases schemas while activity keeps its service, version and operation names.
func TestServiceLifecyclePurgePreservesExecutionNames(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	persistExecutionActivityFixture(t, fixture)
	ctx, pool, s := fixture.ctx, fixture.pool, fixture.repository
	snapshot := serviceContractBatchFixture(2)
	snapshot.ServiceID = fixture.event.ServiceID
	snapshot.ServiceVersionID = uuid.MustParse(fixture.event.ServiceVersionID)
	snapshot.ServiceMetadata.ID = snapshot.ServiceID
	snapshot.ServiceMetadata.ServiceVersionID = snapshot.ServiceVersionID
	snapshot.Version = "2026-07-21"
	// The actual admission path validates the fixture before it can be purged.
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC()
	deadline := observed.Add(time.Hour)
	// Scheduling is advisory until its deadline and can be cancelled without losing schemas.
	requireServiceLifecycleWrite(t, s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Linear", &deadline, false, observed))
	if err := s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Linear", nil, false, observed.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.IsServiceDeleted(ctx, snapshot.ServiceID); err != nil || deleted {
		t.Fatalf("cancelled state: %v %v", deleted, err)
	}
	// An old delayed schedule cannot override the newer cancellation.
	requireServiceLifecycleWrite(t, s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Linear", &deadline, false, observed))
	var scheduled *time.Time
	if err := pool.QueryRow(ctx, `SELECT delete_at FROM fused_service_lifecycle WHERE service_id=$1`, snapshot.ServiceID).Scan(&scheduled); err != nil || scheduled != nil {
		t.Fatalf("stale event: %v %v", scheduled, err)
	}
	// Immediate removal compacts every admitted version and removes execution authority.
	if err := s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Linear", nil, true, observed.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if deleted, err := s.IsServiceDeleted(ctx, snapshot.ServiceID); err != nil || !deleted {
		t.Fatalf("deleted state: %v %v", deleted, err)
	}
	assertPurgedServiceDefinition(t, fixture, snapshot)
	assertPurgedServiceHistory(t, fixture)
}

// assertPurgedServiceDefinition checks payload removal and stale-refresh rejection independently of history rendering.
func assertPurgedServiceDefinition(t *testing.T, fixture executionActivityFixture, snapshot ServiceContractSnapshot) {
	t.Helper()
	ctx, pool, s := fixture.ctx, fixture.pool, fixture.repository
	var payloads, operations int
	err := pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM fused_service_contract_snapshots WHERE service_id=$1)+(SELECT COUNT(*) FROM fused_workspace_services WHERE service_id=$1),(SELECT COUNT(*) FROM fused_deleted_service_operations WHERE service_id=$1)`, snapshot.ServiceID).Scan(&payloads, &operations)
	// Identity-only operation rows must be retained, while full parent snapshots and their children disappear.
	if err != nil || payloads != 0 || operations != 2 {
		t.Fatalf("payloads=%d operations=%d err=%v", payloads, operations, err)
	}
	// A late activation plan must not recreate phantom membership after deletion.
	if _, err = pool.Exec(ctx, `INSERT INTO fused_workspace_services(service_id,service_name,service_slug) VALUES($1,'Deleted','deleted')`, snapshot.ServiceID); err == nil {
		t.Fatal("deleted membership was recreated")
	}
	// An in-flight refresh accepted before deletion cannot recreate the content afterward.
	if _, err = s.UpsertServiceContractSnapshot(ctx, snapshot); err == nil {
		t.Fatal("deleted snapshot was recreated")
	}
}

// assertPurgedServiceHistory uses the canonical activity queries to verify retained names without a full contract.
func assertPurgedServiceHistory(t *testing.T, fixture executionActivityFixture) {
	t.Helper()
	ctx, s := fixture.ctx, fixture.repository
	rows, total, err := s.ListEngineExecutionEventsByApp(ctx, EngineExecutionFilter{AccountID: fixture.event.AccountID, AppFamilyID: fixture.event.AppFamilyID, AppID: fixture.event.AppID, Limit: 1})
	history := requireSingleExecutionEvent(t, rows, total, err)
	// History remains human-readable without using the deleted execution contract.
	if history.ServiceName != "Linear" || history.ServiceVersion != "2026-07-21" || history.EndpointName != fixture.event.EndpointName {
		t.Fatalf("history lost identity: %s %s %s", history.ServiceName, history.ServiceVersion, history.EndpointName)
	}
	analytics, err := s.GetWorkspaceExecutionAnalytics(ctx, fixture.event.AccountID, fixture.event.StartedAt.Add(-time.Minute), fixture.event.StartedAt.Add(time.Minute))
	if err != nil || len(analytics.ByService) != 1 || analytics.ByService[0].Label != "Linear" {
		t.Fatalf("history analytics: %+v %v", analytics.ByService, err)
	}
}

// TestServiceLifecycleDeadlineWorksOffline proves offline calls stop at the deadline while a missed cancellation remains recoverable.
func TestServiceLifecycleDeadlineWorksOffline(t *testing.T) {
	fixture := newExecutionActivityFixture(t)
	ctx, pool, s := fixture.ctx, fixture.pool, fixture.repository
	snapshot := serviceContractBatchFixture(1)
	_, err := s.UpsertServiceContractSnapshot(ctx, snapshot)
	requireServiceLifecycleWrite(t, err)
	observed := time.Now().UTC()
	deadline := observed.Add(time.Hour)
	requireServiceLifecycleWrite(t, s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Offline fixture", &deadline, false, observed))
	// Advance only this disposable deadline; no Registry request is available to the runtime gate.
	_, err = pool.Exec(ctx, `UPDATE fused_service_lifecycle SET delete_at=clock_timestamp()-interval '1 second' WHERE service_id=$1`, snapshot.ServiceID)
	requireServiceLifecycleWrite(t, err)
	deleted, err := s.IsServiceDeleted(ctx, snapshot.ServiceID)
	// A disconnected Engine cannot continue executing past a received deadline.
	if err != nil || !deleted {
		t.Fatalf("deadline gate: %v %v", deleted, err)
	}
	requireServiceLifecycleWrite(t, s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Offline fixture", nil, false, observed.Add(time.Second)))
	deleted, err = s.IsServiceDeleted(ctx, snapshot.ServiceID)
	// A newer cancellation clears the deadline because no Registry removal was committed.
	if err != nil || deleted {
		t.Fatalf("reconnected cancellation: %v %v", deleted, err)
	}
	_, err = s.GetServiceContractMetadata(ctx, snapshot.ServiceID, snapshot.ServiceVersionID)
	requireServiceLifecycleWrite(t, err)
	requireServiceLifecycleWrite(t, s.ApplyServiceLifecycle(ctx, snapshot.ServiceID, "Offline fixture", nil, true, observed.Add(2*time.Second)))
	var count int
	err = pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_service_contract_snapshots WHERE service_id=$1`, snapshot.ServiceID).Scan(&count)
	// Confirmed removal, unlike a potentially superseded offline deadline, permanently compacts the payload.
	if err != nil || count != 0 {
		t.Fatalf("confirmed purge: %d %v", count, err)
	}
}

// requireServiceLifecycleWrite stops a scenario before a failed mutation can invalidate later state assertions.
func requireServiceLifecycleWrite(t *testing.T, err error) {
	t.Helper()
	// Follow-on assertions are meaningful only after a successful committed transition.
	if err != nil {
		t.Fatal(err)
	}
}
