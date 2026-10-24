package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// recoveryMust stops a durability assertion before a failed prerequisite can obscure the actual defect.
func recoveryMust(t *testing.T, err error) {
	t.Helper()
	// Database and encryption failures invalidate all subsequent state assertions.
	if err != nil {
		t.Fatal(err)
	}
}

// TestExecutionRecoverySnapshot proves encrypted continuation, attempt history, and old-owner fencing in production SQL.
func TestExecutionRecoverySnapshot(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "manual", MaxAttempts: 3, BackoffSeconds: 1}
	key := bytes.Repeat([]byte{7}, 32)
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	recoveryMust(t, repository.StartExecutionResult(ctx, record.AccountID, record.AppID, record.ID))
	snapshot, err := repository.ReadExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, key)
	recoveryMust(t, err)
	// An initial attempt has no implicit continuation or inherited execution data.
	if string(snapshot.State) != "null" || snapshot.Revision != 0 {
		t.Fatalf("unexpected initial snapshot: %+v", snapshot)
	}
	revision, err := repository.SaveExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, 0, json.RawMessage(`{"cursor":2,"private":"saved-progress"}`), key)
	recoveryMust(t, err)
	// Only the committed revision authorizes the next save.
	if revision != 1 {
		t.Fatalf("revision=%d", revision)
	}
	_, err = repository.SaveExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, 0, json.RawMessage(`{}`), key)
	// A stale writer cannot replace acknowledged progress.
	if !errors.Is(err, ErrExecutionRecoveryConflict) {
		t.Fatalf("stale snapshot accepted: %v", err)
	}
	var encrypted string
	recoveryMust(t, pool.QueryRow(ctx, `SELECT encrypted_snapshot FROM fused_unified_app_recovery WHERE execution_id=$1`, record.ID).Scan(&encrypted))
	// Continuation values must not be queryable plaintext in the recovery row.
	if strings.Contains(encrypted, "saved-progress") {
		t.Fatal("snapshot stored as plaintext")
	}
	recoveryMust(t, repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "failed", nil, json.RawMessage(`null`), "execution_failed", "failed"))
	assertRecoveryContinuation(t, ctx, repository, record, key)
}

// TestExecutionRecoveryCrashRace checks natural failure policy separately from concurrent replica ownership.
func TestExecutionRecoveryCrashRace(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 2, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	recoveryMust(t, repository.StartExecutionResult(ctx, record.AccountID, record.AppID, record.ID))
	// This focused SQL test advances only its own lease; process tests exercise actual runtime interruption.
	_, err := pool.Exec(ctx, `UPDATE fused_unified_app_recovery SET lease_until=clock_timestamp()-INTERVAL '1 second' WHERE execution_id=$1`, record.ID)
	recoveryMust(t, err)
	var wg sync.WaitGroup
	winners := make(chan uuid.UUID, 8)
	// Independent keys simulate a manual trigger racing automatic recovery across replicas.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		// Each contender must reserve against the authoritative database rather than local locks.
		go func(i int) {
			defer wg.Done()
			request := ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, Automatic: i%2 == 0, KeyHash: fmt.Sprintf("%064x", i+1)}
			next, created, err := repository.ReserveExecutionAttempt(ctx, request)
			// Conflicts are expected losers; other failures indicate broken persistence.
			if err != nil && !errors.Is(err, ErrExecutionRecoveryConflict) {
				t.Error(err)
				return
			}
			if created {
				winners <- next.ID
			} // Only a committed reservation can authorize a worker.
		}(i)
	}
	wg.Wait()
	close(winners)
	var ids []uuid.UUID
	// Collect only actual owners, not successful duplicate lookups.
	for id := range winners {
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		t.Fatalf("replacement owners=%d", len(ids))
	} // A logical run permits exactly one current attempt.
	recoveryMust(t, repository.StartExecutionResult(ctx, record.AccountID, record.AppID, ids[0]))
	recoveryMust(t, repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, ids[0], "failed", nil, json.RawMessage(`null`), "execution_failed", "failed"))
	_, _, err = repository.ReserveExecutionAttempt(ctx, ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: ids[0], KeyHash: strings.Repeat("b", 64)})
	// Manual triggers cannot evade the app's total attempt budget.
	if !errors.Is(err, ErrExecutionRecoveryConflict) {
		t.Fatalf("attempt budget bypassed: %v", err)
	}
}

// TestExecutionRecoveryIsolation checks that snapshots cannot outlive tenant or version authority.
func TestExecutionRecoveryIsolation(t *testing.T) {
	repository, pool, ctx, record, second := newUnifiedWebhookDatabase(t)
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 3, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	recoveryMust(t, repository.StartExecutionResult(ctx, record.AccountID, record.AppID, record.ID))
	_, err := repository.ReadExecutionSnapshot(ctx, uuid.New(), record.AppID, record.ID, bytes.Repeat([]byte{7}, 32))
	// Possessing an execution ID is insufficient to read another account's progress.
	if err == nil {
		t.Fatal("cross-account snapshot read accepted")
	}
	_, err = pool.Exec(ctx, `UPDATE fused_app_families SET unified_active_app_id=$2 WHERE app_family_id=$1`, record.AppFamilyID, second)
	recoveryMust(t, err)
	// A retained snapshot must not revive a version that stopped receiving traffic.
	if err := repository.CheckExecutionAttempt(ctx, record.AccountID, record.AppID, record.ID); err == nil {
		t.Fatal("retired version retained authority")
	}
}

// TestExecutionRecoveryAutomaticBackoff proves ordinary failure is discoverable only after its policy delay.
func TestExecutionRecoveryAutomaticBackoff(t *testing.T) {
	repository, _, ctx, record, _ := newUnifiedWebhookDatabase(t)
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 2, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	recoveryMust(t, repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "failed", nil, json.RawMessage(`null`), "execution_failed", "failed"))
	request := ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, KeyHash: strings.Repeat("a", 64), Automatic: true}
	_, _, err := repository.ReserveExecutionAttempt(ctx, request)
	// Immediate failures cannot create a hot retry loop.
	if !errors.Is(err, ErrExecutionRecoveryConflict) {
		t.Fatalf("backoff bypassed: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	next, created, err := repository.ReserveExecutionAttempt(ctx, request)
	recoveryMust(t, err)
	if !created {
		t.Fatal("automatic recovery not admitted after delay")
	} // A finite delay must eventually permit recovery.
	snapshot, err := repository.ReadExecutionSnapshot(context.Background(), next.AccountID, next.AppID, next.ID, bytes.Repeat([]byte{7}, 32))
	recoveryMust(t, err)
	// Runs without a snapshot start again from original input and explicit null progress.
	if string(snapshot.State) != "null" {
		t.Fatal("invented continuation")
	}
}

// assertRecoveryContinuation verifies retry identity, inherited progress and stale-owner rejection.
func assertRecoveryContinuation(t *testing.T, ctx context.Context, repository *postgresStore, record ExecutionResult, key []byte) {
	t.Helper()
	request := ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, KeyHash: strings.Repeat("a", 64)}
	next, created, err := repository.ReserveExecutionAttempt(ctx, request)
	recoveryMust(t, err)
	// A recovery attempt has its own immutable receipt while remaining in the same logical run.
	if !created || next.ID == record.ID {
		t.Fatal("replacement not reserved")
	}
	repeated, created, err := repository.ReserveExecutionAttempt(ctx, request)
	recoveryMust(t, err)
	// Lost HTTP replies cannot cause another attempt or repeat provider work.
	if created || repeated.ID != next.ID {
		t.Fatal("manual retry was not idempotent")
	}
	recoveryMust(t, repository.StartExecutionResult(ctx, next.AccountID, next.AppID, next.ID))
	snapshot, err := repository.ReadExecutionSnapshot(ctx, next.AccountID, next.AppID, next.ID, key)
	recoveryMust(t, err)
	// A fresh attempt reads committed continuation, without retaining the first execution host.
	if snapshot.Revision != 1 || !strings.Contains(string(snapshot.State), "saved-progress") {
		t.Fatalf("lost snapshot: %+v", snapshot)
	}
	_, err = repository.SaveExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, 1, json.RawMessage(`{}`), key)
	// The previous attempt remains fenced even with a valid snapshot revision and Engine key.
	if !errors.Is(err, ErrExecutionRecoveryConflict) {
		t.Fatalf("old owner accepted: %v", err)
	}
	assertRecoveryHistory(t, ctx, repository, record)
}

// assertRecoveryHistory verifies the logical grouping without exposing continuation values.
func assertRecoveryHistory(t *testing.T, ctx context.Context, repository *postgresStore, record ExecutionResult) {
	t.Helper()
	history, err := repository.ListExecutionAttempts(ctx, record.AccountID, record.AppID, record.ID)
	recoveryMust(t, err)
	// The original read capability can discover the current attempt without exposing snapshots.
	if len(history) != 2 || history[0].Current || !history[1].Current || history[1].RunID != record.ID {
		t.Fatalf("history=%+v", history)
	}
}

// TestExecutionRecoveryQueuedOwnership keeps delayed management work durable and grants it to only one scheduler.
func TestExecutionRecoveryQueuedOwnership(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "manual", MaxAttempts: 2, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	recoveryMust(t, repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "failed", nil, json.RawMessage(`null`), "execution_failed", "failed"))
	next, _, err := repository.ReserveExecutionAttempt(ctx, ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, KeyHash: strings.Repeat("a", 64), Enqueue: true})
	recoveryMust(t, err)
	// Queue admission itself is not authority to dispatch provider work.
	if err := repository.CheckExecutionAttempt(ctx, next.AccountID, next.AppID, next.ID); err == nil {
		t.Fatal("unclaimed queue entry acquired authority")
	}
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET updated_at=NOW()-INTERVAL '10 minutes' WHERE id=$1`, next.ID)
	recoveryMust(t, err)
	claimed, err := repository.ClaimQueuedExecutionAttempt(ctx, next.AccountID, next.AppID, next.ID)
	recoveryMust(t, err)
	if !claimed {
		t.Fatal("queued attempt was not claimed")
	} // One scheduler must acquire the durable request.
	repeated, err := repository.ClaimQueuedExecutionAttempt(ctx, next.AccountID, next.AppID, next.ID)
	recoveryMust(t, err)
	if repeated {
		t.Fatal("queued attempt was claimed twice")
	} // A replica cannot reuse another owner's claim.
	_, err = repository.RecoverStaleExecutionResults(ctx, time.Now().Add(-2*time.Minute), 500)
	recoveryMust(t, err)
	current, err := repository.GetExecutionResult(ctx, next.AccountID, next.AppID, next.ID)
	recoveryMust(t, err)
	if current.Status != "queued" {
		t.Fatal("cleanup invalidated a newly leased attempt")
	} // Lease age, not queue waiting time, determines abandonment.
}

// TestExecutionRecoveryReloadsTokenPolicy prevents automatic recovery from widening or retaining revoked authority.
func TestExecutionRecoveryReloadsTokenPolicy(t *testing.T) {
	repository, _, ctx, record, _ := newUnifiedWebhookDatabase(t)
	record.AppTokenID = uuid.New()
	record.SourceWebhookEventID = ""
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 2, BackoffSeconds: 1}
	_, err := repository.CreateAppToken(ctx, AppTokenIssue{ID: record.AppTokenID, AppFamilyID: record.AppFamilyID, TokenHash: strings.Repeat("c", 64), Name: "recovery", BindingMode: AppTokenBindingDynamic, Policy: AppTokenPolicy{AllowedOperations: []string{"selected.operation"}}})
	recoveryMust(t, err)
	recoveryMust(t, repository.CreateExecutionResult(ctx, record))
	projection, err := repository.AuthorizeExecutionRecovery(ctx, record.AccountID, record.AppID, record.ID)
	recoveryMust(t, err)
	// Automatic attempts must preserve exact grants rather than creating an unrestricted synthetic token.
	if projection.TokenPolicy.AllowAll || !projection.TokenPolicy.AllowsOperation("selected.operation") || projection.TokenPolicy.AllowsOperation("other.operation") {
		t.Fatal("token policy changed during recovery")
	}
	_, err = repository.RevokeAppToken(ctx, record.AppFamilyID, "recovery")
	recoveryMust(t, err)
	_, err = repository.AuthorizeExecutionRecovery(ctx, record.AccountID, record.AppID, record.ID)
	if err == nil {
		t.Fatal("revoked execution authority survived recovery")
	} // Historical inputs and snapshots do not substitute for live authority.
}

// TestAutomaticScheduleRecoveryPreservesRecurrence ensures crash classification does not silently disable opted-in cron.
func TestAutomaticScheduleRecoveryPreservesRecurrence(t *testing.T) {
	repository, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
	key := bytes.Repeat([]byte{7}, 32)
	item := UnifiedAppSchedule{ID: uuid.New(), AccountID: record.AccountID, FamilyID: record.AppFamilyID, Name: "Recovery", Input: record.Input, Cron: "0 9 * * *", Timezone: "UTC", Enabled: true, CreatedBy: uuid.New()}
	// This schedule must be removed before its enclosing app family fixture is deleted.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_unified_app_schedules WHERE id=$1`, item.ID)
	})
	recoveryMust(t, repository.SaveUnifiedAppSchedule(ctx, item, key))
	_, err := pool.Exec(ctx, `UPDATE fused_unified_app_schedules SET next_run_at=NOW()-INTERVAL '1 second' WHERE id=$1`, item.ID)
	recoveryMust(t, err)
	items, err := repository.ListUnifiedAppSchedules(ctx, record.AccountID, record.AppFamilyID, key)
	recoveryMust(t, err)
	item = items[0]
	record.SourceWebhookEventID = ""
	record.SourceScheduleID = &item.ID
	record.Recovery = &ExecutionRecoveryPolicy{Mode: "automatic", MaxAttempts: 3, BackoffSeconds: 1}
	created, err := repository.ReserveScheduledExecution(ctx, item, record)
	recoveryMust(t, err)
	if !created {
		t.Fatal("schedule was not reserved")
	} // Only a persisted occurrence can enter recovery.
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_recovery SET lease_until=NOW()-INTERVAL '1 second' WHERE execution_id=$1`, record.ID)
	recoveryMust(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_schedules SET last_started_at=NOW()-INTERVAL '6 minutes' WHERE id=$1`, item.ID)
	recoveryMust(t, err)
	_, err = repository.RecoverScheduledExecutions(ctx)
	recoveryMust(t, err)
	items, err = repository.ListUnifiedAppSchedules(ctx, record.AccountID, record.AppFamilyID, key)
	recoveryMust(t, err)
	// Explicit automatic policy preserves the user's recurrence choice after an abandoned attempt.
	if !items[0].Enabled || items[0].LastStatus != "indeterminate" {
		t.Fatalf("schedule=%+v", items[0])
	}
}
