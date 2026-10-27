package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestExecutionResultConfiguredRetention checks finite and indefinite payload retention through real PostgreSQL.
func TestExecutionResultConfiguredRetention(t *testing.T) {
	// Include the hosted default and an operator period exceeding a billing-plan history window.
	for _, days := range []int{7, 90, 0} {
		// Each retention setting owns an isolated result and recovery chain.
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			_, pool, ctx, record, _ := newUnifiedWebhookDatabase(t)
			repository := NewPostgresStore(pool, WithExecutionResultRetentionDays(days)).(*postgresStore)
			record.Recovery = &ExecutionRecoveryPolicy{Mode: "manual", MaxAttempts: 3, BackoffSeconds: 1}
			key := bytes.Repeat([]byte{7}, 32)
			recoveryMust(t, repository.CreateExecutionResult(ctx, record))
			_, err := repository.SaveExecutionSnapshot(ctx, record.AccountID, record.AppID, record.ID, 0, json.RawMessage(`{"next":1}`), key)
			recoveryMust(t, err)
			recoveryMust(t, repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "failed", nil, json.RawMessage(`null`), "failed", "failed"))
			recoveryMust(t, repository.SaveExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, json.RawMessage(`{"error":"private"}`), key))
			recoveryMust(t, repository.SaveReplayEvidence(ctx, record.AccountID, record.AppID, record.ID, json.RawMessage(`[]`), key))
			loaded, err := repository.GetExecutionResult(ctx, record.AccountID, record.AppID, record.ID)
			recoveryMust(t, err)
			// Zero deliberately exposes no expiry; finite retention starts at completion.
			if days == 0 {
				if loaded.ExpiresAt != nil {
					t.Fatal("indefinite result has an expiry")
				}
			} else if loaded.ExpiresAt == nil || loaded.ExpiresAt.Sub(*loaded.CompletedAt) != time.Duration(days)*24*time.Hour {
				t.Fatalf("unexpected retention: %+v", loaded)
			}
			var same bool
			recoveryMust(t, pool.QueryRow(ctx, `SELECT e.expires_at IS NOT DISTINCT FROM r.expires_at FROM fused_unified_app_results r JOIN fused_unified_app_replay_evidence e ON e.execution_id=r.id WHERE r.id=$1`, record.ID).Scan(&same))
			// Replay evidence must never expire sooner than the result it explains.
			if !same {
				t.Fatal("evidence retention diverged")
			}
			_, err = repository.GetExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, key)
			recoveryMust(t, err)
			_, err = repository.GetReplayEvidence(ctx, record.AccountID, record.AppID, record.ID, key)
			recoveryMust(t, err)
			// Simulate a two-day-old failure, including its encrypted evidence, without waiting for wall time.
			_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=completed_at-INTERVAL '2 days',expires_at=expires_at-INTERVAL '2 days' WHERE id=$1`, record.ID)
			recoveryMust(t, err)
			_, err = pool.Exec(ctx, `UPDATE fused_unified_app_replay_evidence SET expires_at=expires_at-INTERVAL '2 days' WHERE execution_id=$1`, record.ID)
			recoveryMust(t, err)
			_, err = repository.DeleteExpiredExecutionResults(ctx, time.Now().AddDate(10, 0, 0), 1000)
			recoveryMust(t, err)
			_, err = repository.GetExecutionResult(ctx, record.AccountID, record.AppID, record.ID)
			recoveryMust(t, err)
			next, _, err := repository.ReserveExecutionAttempt(ctx, ExecutionRerunRequest{AccountID: record.AccountID, AppID: record.AppID, SourceID: record.ID, KeyHash: fmt.Sprintf("%064x", 1)})
			recoveryMust(t, err)
			snapshot, err := repository.ReadExecutionSnapshot(ctx, next.AccountID, next.AppID, next.ID, key)
			recoveryMust(t, err)
			// Transaction-scoped writers must preserve snapshots under every retention setting.
			if snapshot.Revision != 1 {
				t.Fatal("snapshot lost across retention-aware reservation")
			}
			recoveryMust(t, repository.CompleteExecutionResult(ctx, next.AccountID, next.AppID, next.ID, "succeeded", json.RawMessage(`{}`), json.RawMessage(`null`), "", ""))
		})
	}
}
