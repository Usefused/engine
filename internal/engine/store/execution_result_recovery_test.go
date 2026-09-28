package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestRecoverStaleExecutionResultsBounds rejects maintenance work beyond one safe batch.
func TestRecoverStaleExecutionResultsBounds(t *testing.T) {
	repository := &postgresStore{}
	// Invalid limits must fail before opening a database connection.
	if _, err := repository.RecoverStaleExecutionResults(context.Background(), time.Now(), 0); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("expected invalid recovery limit, got %v", err)
	}
}

// TestRecoverStaleExecutionResultsPostgres classifies crash states without dispatching providers.
func TestRecoverStaleExecutionResultsPostgres(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	// Recovery needs an explicitly supplied isolated PostgreSQL instance.
	if url == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, url)
	if err != nil {
		t.Fatalf("initialize Engine database: %v", err)
	}
	defer pool.Close()
	repository := NewPostgresStore(pool).(interface {
		ExecutionResultStore
		ExecutionResultRecoveryStore
	})
	queued := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(), AppVersion: "1", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64), Input: json.RawMessage(`{}`)}
	running := queued
	running.ID = uuid.New()
	defer pool.Exec(context.Background(), `DELETE FROM fused_execution_app_results WHERE id IN ($1,$2)`, queued.ID, running.ID)
	if err := repository.CreateExecutionResult(ctx, queued); err != nil {
		t.Fatalf("create queued execution: %v", err)
	}
	if err := repository.CreateExecutionResult(ctx, running); err != nil {
		t.Fatalf("create running execution: %v", err)
	}
	if err := repository.StartExecutionResult(ctx, running.AccountID, running.AppID, running.ID); err != nil {
		t.Fatalf("start running execution: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_execution_app_results SET updated_at=NOW()-INTERVAL '3 minutes' WHERE id IN ($1,$2)`, queued.ID, running.ID)
	if err != nil {
		t.Fatalf("age test executions: %v", err)
	}
	if _, err := repository.RecoverStaleExecutionResults(ctx, time.Now().Add(-2*time.Minute), 500); err != nil {
		t.Fatalf("recover stale executions: %v", err)
	}
	assertRecoveredExecution(t, ctx, repository, queued, "failed")
	assertRecoveredExecution(t, ctx, repository, running, "indeterminate")
}

// assertRecoveredExecution checks one bounded crash outcome and its new retention window.
func assertRecoveredExecution(t *testing.T, ctx context.Context, repository ExecutionResultStore, source ExecutionResult, status string) {
	t.Helper()
	record, err := repository.GetExecutionResult(ctx, source.AccountID, source.AppID, source.ID)
	if err != nil {
		t.Fatalf("get recovered execution: %v", err)
	}
	// Recovery must retain the result for callers without inventing a successful provider outcome.
	if record.Status != status || record.CompletedAt == nil || record.ExpiresAt == nil || record.ExpiresAt.Before(record.CompletedAt.Add(24*time.Hour-time.Minute)) {
		t.Fatalf("invalid recovered result: status=%q completed=%v expires=%v", record.Status, record.CompletedAt, record.ExpiresAt)
	}
}
