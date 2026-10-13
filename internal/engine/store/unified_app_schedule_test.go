package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestScheduleCalendar proves recurrence uses the named zone, including missing and repeated daylight-saving hours.
func TestScheduleCalendar(t *testing.T) {
	cases := []struct{ cron, zone, after, want string }{
		{"0 9 * * 1-5", "Europe/London", "2026-10-12T07:00:00Z", "2026-10-12T08:00:00Z"},
		{"30 1 * * *", "Europe/London", "2026-03-29T00:00:00Z", "2026-03-30T00:30:00Z"},
		{"30 1 * * *", "Europe/London", "2026-10-25T00:30:00Z", "2026-10-25T01:30:00Z"},
		{"*/5 * * * *", "UTC", "2026-10-12T09:01:00Z", "2026-10-12T09:05:00Z"},
	}
	// Fixed clocks test actual calendar outcomes rather than echoing parser internals.
	for _, item := range cases {
		after, _ := time.Parse(time.RFC3339, item.after)
		next, err := NextScheduleTime(item.cron, item.zone, after)
		if err != nil || next.Format(time.RFC3339) != item.want {
			t.Fatalf("%+v: %v %v", item, next, err)
		}
	}
	// Seconds, embedded zones, impossible dates, and host-dependent zones must fail closed.
	for _, item := range [][2]string{{"0 0 30 2 *", "UTC"}, {"* * * * * *", "UTC"}, {"CRON_TZ=UTC 0 9 * * *", "UTC"}, {"* * * * *", "Local"}, {"* * * * *", "bad/zone"}} {
		if _, err := NextScheduleTime(item[0], item[1], time.Now()); err == nil {
			t.Fatalf("accepted %v", item)
		}
	}
}

// TestScheduleValidation prevents missing, ambiguous, past, and oversized scheduled inputs.
func TestScheduleValidation(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	base := UnifiedAppSchedule{ID: uuid.New(), AccountID: uuid.New(), FamilyID: uuid.New(), Name: "One time", Input: json.RawMessage(`{"value":1}`), Timezone: "UTC", RunAt: &future, Enabled: true}
	if err := ValidateSchedule(&base, now); err != nil || !base.NextRunAt.Equal(future) {
		t.Fatalf("valid schedule rejected: %v", err)
	}
	// Each invalid variant must be rejected before any durable automation exists.
	for _, change := range []func(*UnifiedAppSchedule){
		func(item *UnifiedAppSchedule) { item.Cron = "* * * * *" },                    // Two timing modes would make execution ambiguous.
		func(item *UnifiedAppSchedule) { item.RunAt = &now },                          // An enabled one-time date must be deliberately in the future.
		func(item *UnifiedAppSchedule) { item.Input = json.RawMessage(`{"broken":`) }, // Broken JSON cannot reach authored validation.
		func(item *UnifiedAppSchedule) {
			item.Input = json.RawMessage(`"` + strings.Repeat("x", MaxExecutionDataBytes) + `"`)
		}, // Large inputs cannot bypass execution bounds.
	} {
		item := base
		change(&item)
		if err := ValidateSchedule(&item, now); err == nil {
			t.Fatal("invalid schedule accepted")
		}
	}
}

// TestScheduledResultProvenance admits explicit automation while rejecting forged caller or webhook attribution.
func TestScheduledResultProvenance(t *testing.T) {
	id := uuid.New()
	record := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.0.0", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64), SourceScheduleID: &id}
	if err := validateNewExecutionResult(record); err != nil {
		t.Fatal(err)
	}
	record.AppTokenID = uuid.New()
	if err := validateNewExecutionResult(record); err == nil {
		t.Fatal("token-backed schedule admitted")
	}
	record.AppTokenID = uuid.Nil
	record.SourceWebhookEventID = "event"
	if err := validateNewExecutionResult(record); err == nil {
		t.Fatal("two automatic triggers admitted")
	}
}

// TestSchedulePostgresReservation proves encryption, cross-tenant isolation, and single ownership across replica races.
func TestSchedulePostgresReservation(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	key := bytes.Repeat([]byte{7}, 32)
	item := UnifiedAppSchedule{ID: uuid.New(), AccountID: record.AccountID, FamilyID: record.AppFamilyID, Name: "Daily", Input: json.RawMessage(`{"private":"schedule-secret"}`), Cron: "0 9 * * *", Timezone: "Europe/London", Enabled: true, CreatedBy: uuid.New()}
	// Family cleanup must happen only after this test's schedule is removed.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_unified_app_schedules WHERE id=$1`, item.ID)
	})
	if err := repository.SaveUnifiedAppSchedule(ctx, item, key); err != nil {
		t.Fatal(err)
	}
	assertSchedulePrivacy(t, ctx, repository, pool, item, key)
	// Simulate missed times without sleeps or manipulating the process clock.
	if _, err := pool.Exec(ctx, `UPDATE fused_unified_app_schedules SET next_run_at=NOW()-INTERVAL '2 days' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	due, err := repository.DueUnifiedAppSchedules(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	// Discovery can include other fixtures, but only this exact schedule belongs to this race.
	for _, candidate := range due {
		if candidate.ID == item.ID {
			item = candidate
		}
	}
	if item.NextRunAt == nil {
		t.Fatal("due schedule missing")
	}
	record.SourceWebhookEventID = ""
	record.SourceScheduleID = &item.ID
	record.Input = item.Input
	assertScheduleRace(t, ctx, repository, item, record)
	current := assertScheduleResult(t, ctx, repository, item, record, key)
	assertScheduleRecovery(t, ctx, repository, pool, item, current, key)
}

// assertSchedulePrivacy tests tenant isolation and verifies saved input is never stored as plaintext.
func assertSchedulePrivacy(t *testing.T, ctx context.Context, repository *postgresStore, pool *pgxpool.Pool, item UnifiedAppSchedule, key []byte) {
	t.Helper()
	var encrypted string
	if err := pool.QueryRow(ctx, `SELECT encrypted_input FROM fused_unified_app_schedules WHERE id=$1`, item.ID).Scan(&encrypted); err != nil || strings.Contains(encrypted, "schedule-secret") {
		t.Fatal("input was not encrypted", err)
	}
	foreign, err := repository.ListUnifiedAppSchedules(ctx, uuid.New(), item.FamilyID, key)
	if err != nil || len(foreign) != 0 {
		t.Fatal("cross-tenant schedule disclosure", err)
	}
	if err := repository.DeleteUnifiedAppSchedule(ctx, uuid.New(), item.FamilyID, item.ID, 1); !errors.Is(err, ErrScheduleConflict) {
		t.Fatal("cross-tenant mutation", err)
	}

}

// assertScheduleRace models independent replicas trying to own the same observed occurrence.
func assertScheduleRace(t *testing.T, ctx context.Context, repository *postgresStore, item UnifiedAppSchedule, record ExecutionResult) {
	t.Helper()
	var wins atomic.Int32
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		// Independent result IDs model different replicas admitting the same observed occurrence.
		go func() {
			defer group.Done()
			run := record
			run.ID = uuid.New()
			created, err := repository.ReserveScheduledExecution(ctx, item, run)
			if err != nil {
				t.Error(err)
			}
			if created {
				wins.Add(1)
			}
		}()
	}
	group.Wait()
	if wins.Load() != 1 {
		t.Fatalf("executed %d times", wins.Load())
	}

}

// assertScheduleResult checks that result provenance and future recurrence committed together.
func assertScheduleResult(t *testing.T, ctx context.Context, repository *postgresStore, item UnifiedAppSchedule, record ExecutionResult, key []byte) UnifiedAppSchedule {
	t.Helper()
	items, err := repository.ListUnifiedAppSchedules(ctx, item.AccountID, item.FamilyID, key)
	if err != nil || len(items) != 1 || items[0].LastExecutionID == nil || !items[0].NextRunAt.After(time.Now()) {
		t.Fatalf("missing durable advancement: %+v %v", items, err)
	}
	current := items[0]
	result, err := repository.GetExecutionResult(ctx, item.AccountID, record.AppID, *current.LastExecutionID)
	if err != nil || result.SourceScheduleID == nil || *result.SourceScheduleID != item.ID {
		t.Fatalf("lost result provenance: %+v %v", result, err)
	}

	return current
}

// assertScheduleRecovery protects admitted input from edits and pauses abandoned provider work.
func assertScheduleRecovery(t *testing.T, ctx context.Context, repository *postgresStore, pool *pgxpool.Pool, item, current UnifiedAppSchedule, key []byte) {
	t.Helper()
	// Neither stale edits nor a new edit during execution may replace an admitted occurrence's input.
	if err := repository.SaveUnifiedAppSchedule(ctx, item, key); !errors.Is(err, ErrScheduleConflict) {
		t.Fatalf("stale edit: %v", err)
	}
	if err := repository.DeleteUnifiedAppSchedule(ctx, item.AccountID, item.FamilyID, item.ID, current.Revision); !errors.Is(err, ErrScheduleConflict) {
		t.Fatalf("running delete: %v", err)
	}
	if err := repository.FinishScheduledExecution(ctx, item.ID, *current.LastExecutionID, "succeeded"); err != nil {
		t.Fatal(err)
	}
	// An abandoned subsequent run must pause without becoming eligible for automatic replay.
	if _, err := pool.Exec(ctx, `UPDATE fused_unified_app_schedules SET last_status='running',last_started_at=NOW()-INTERVAL '6 minutes' WHERE id=$1`, item.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.RecoverScheduledExecutions(ctx); err != nil {
		t.Fatal(err)
	}
	items, err := repository.ListUnifiedAppSchedules(ctx, item.AccountID, item.FamilyID, key)
	if err != nil || items[0].Enabled || items[0].LastStatus != "indeterminate" {
		t.Fatal("uncertain run did not pause", err)
	}
}

// TestScheduleOneTimeReservation rolls back failed admission and leaves a completed one-time occurrence without a next run.
func TestScheduleOneTimeReservation(t *testing.T) {
	repository, pool, ctx, record, secondID := newUnifiedWebhookDatabase(t)
	key := bytes.Repeat([]byte{9}, 32)
	future := time.Now().Add(time.Hour)
	item := UnifiedAppSchedule{ID: uuid.New(), AccountID: record.AccountID, FamilyID: record.AppFamilyID, Name: "Once", Input: json.RawMessage(`{}`), RunAt: &future, Timezone: "UTC", Enabled: true, CreatedBy: uuid.New()}
	// Remove only this test's schedule before the family fixture is cleaned up.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_unified_app_schedules WHERE id=$1`, item.ID)
	})
	requireScheduleSuccess(t, repository.SaveUnifiedAppSchedule(ctx, item, key))
	_, err := pool.Exec(ctx, `UPDATE fused_unified_app_schedules SET next_run_at=NOW()-INTERVAL '1 minute' WHERE id=$1`, item.ID)
	requireScheduleSuccess(t, err)
	items, err := repository.ListUnifiedAppSchedules(ctx, item.AccountID, item.FamilyID, key)
	requireScheduleSuccess(t, err)
	item = items[0]
	record.SourceWebhookEventID = ""
	record.SourceScheduleID = &item.ID
	record.Input = item.Input
	assertScheduleRollback(t, ctx, repository, item, record, key)
	created, err := repository.ReserveScheduledExecution(ctx, item, record)
	if err != nil || !created {
		t.Fatalf("one-time admission failed: %v", err)
	}
	created, err = repository.ReserveScheduledExecution(ctx, item, record)
	if err != nil || created {
		t.Fatalf("one-time occurrence admitted twice: %v", err)
	}
	items, err = repository.ListUnifiedAppSchedules(ctx, item.AccountID, item.FamilyID, key)
	if err != nil || items[0].NextRunAt != nil {
		t.Fatal("one-time run retained a next occurrence", err)
	}
	// A retained old version cannot accept new work after another version receives traffic.
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1`, item.FamilyID, secondID)
	requireScheduleSuccess(t, err)
	_, err = repository.ReserveScheduledExecution(ctx, item, record)
	if !errors.Is(err, ErrAppDeactivated) {
		t.Fatalf("stale traffic admitted: %v", err)
	}
}

// assertScheduleRollback proves a rejected result cannot consume or advance an occurrence.
func assertScheduleRollback(t *testing.T, ctx context.Context, repository *postgresStore, item UnifiedAppSchedule, record ExecutionResult, key []byte) {
	t.Helper()
	record.ReadHandleHash = "invalid"
	created, err := repository.ReserveScheduledExecution(ctx, item, record)
	if err == nil || created {
		t.Fatal("invalid result admitted")
	}
	items, err := repository.ListUnifiedAppSchedules(ctx, item.AccountID, item.FamilyID, key)
	if err != nil || items[0].LastExecutionID != nil || !items[0].NextRunAt.Equal(*item.NextRunAt) {
		t.Fatal("failed result consumed occurrence", err)
	}
}

// requireScheduleSuccess keeps fixture setup failures separate from the concurrency invariants under test.
func requireScheduleSuccess(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
