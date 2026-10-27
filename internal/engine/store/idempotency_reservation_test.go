package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestIdempotencyReservationConcurrentOwners proves the SQL unique key fences dispatch across replicas.
func TestIdempotencyReservationConcurrentOwners(t *testing.T) {
	repository, _, ctx, result, _ := newUnifiedWebhookDatabase(t)
	var wg sync.WaitGroup
	owners := make(chan *models.IdempotentExecution, 12)
	failures := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		// Each goroutine models a separate Engine attempting the same provider call.
		go func() {
			defer wg.Done()
			record := &models.IdempotentExecution{ID: uuid.New(), AppID: result.AppID, IdempotencyKeyHash: "same-key", RequestBodyHash: "same-request", ExpiresAt: time.Now().Add(time.Hour)}
			err := repository.ReserveIdempotentExecution(ctx, record)
			// Exactly one successful reservation grants dispatch authority.
			if err == nil {
				owners <- record
				return
			}
			// Unique-key races are expected; SQL failures must remain visible.
			if !errors.Is(err, ErrIdempotencyInProgress) {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(owners)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	// There must be exactly one durable owner among all concurrent replicas.
	if len(owners) != 1 {
		t.Fatalf("owners=%d", len(owners))
	}
	owner := <-owners
	_, err := repository.GetIdempotentExecution(ctx, owner.AppID, owner.IdempotencyKeyHash, owner.RequestBodyHash)
	// The pending row cannot masquerade as a successful empty response.
	if !errors.Is(err, ErrIdempotencyInProgress) {
		t.Fatalf("pending lookup=%v", err)
	}
	owner.ResponseBody = []byte(`{"created":true}`)
	owner.ResponseStatus = 201
	owner.ResponseMediaFamily = "json"
	recoveryMust(t, repository.SaveIdempotentExecution(ctx, owner))
	got, err := repository.GetIdempotentExecution(ctx, owner.AppID, owner.IdempotencyKeyHash, owner.RequestBodyHash)
	recoveryMust(t, err)
	// Only the winning owner's exact bytes and response contract may be replayed.
	if string(got.ResponseBody) != string(owner.ResponseBody) || got.ResponseStatus != 201 {
		t.Fatalf("response=%+v", got)
	}
	_, err = repository.GetIdempotentExecution(ctx, owner.AppID, owner.IdempotencyKeyHash, "changed-request")
	// Inputs remain protected even after a successful reservation completes.
	if !errors.Is(err, ErrIdempotencyKeyConflict) {
		t.Fatalf("changed request=%v", err)
	}
}

// TestSnapshotProviderResponseRetention keeps responses for the logical run rather than an expired original attempt.
func TestSnapshotProviderResponseRetention(t *testing.T) {
	repository, pool, ctx, result, _ := newUnifiedWebhookDatabase(t)
	result.Recovery = &ExecutionRecoveryPolicy{Mode: "manual", MaxAttempts: 3, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, result))
	response := &models.IdempotentExecution{ID: uuid.New(), AppID: result.AppID, RunID: &result.ID, IdempotencyKeyHash: "run-key", RequestBodyHash: "body", ExpiresAt: time.Now().Add(-time.Hour), ResponseBody: []byte(`{"id":7}`), ResponseStatus: 200, ResponseMediaFamily: "json"}
	recoveryMust(t, repository.ReserveIdempotentExecution(ctx, response))
	recoveryMust(t, repository.SaveIdempotentExecution(ctx, response))
	recoveryMust(t, repository.CompleteExecutionResult(ctx, result.AccountID, result.AppID, result.ID, "failed", nil, json.RawMessage(`null`), "failed", "failed"))
	next, _, err := repository.ReserveExecutionAttempt(ctx, ExecutionRerunRequest{AccountID: result.AccountID, AppID: result.AppID, SourceID: result.ID, KeyHash: fmt.Sprintf("%064x", 1)})
	recoveryMust(t, err)
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '3 days',expires_at=NOW()-INTERVAL '2 days' WHERE id=$1`, result.ID)
	recoveryMust(t, err)
	_, err = repository.DeleteExpiredExecutionResults(ctx, time.Now(), 1000)
	recoveryMust(t, err)
	got, err := NewPostgresStore(pool).GetIdempotentExecution(ctx, result.AppID, response.IdempotencyKeyHash, response.RequestBodyHash)
	recoveryMust(t, err)
	// A current retry retains responses even after both the generic TTL and the original result expire.
	if string(got.ResponseBody) != `{"id":7}` {
		t.Fatalf("response=%+v", got)
	}
	recoveryMust(t, repository.CompleteExecutionResult(ctx, next.AccountID, next.AppID, next.ID, "succeeded", json.RawMessage(`{}`), json.RawMessage(`null`), "", ""))
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '3 days',expires_at=NOW()-INTERVAL '2 days' WHERE id=$1`, next.ID)
	recoveryMust(t, err)
	_, err = repository.DeleteExpiredExecutionResults(ctx, time.Now(), 1000)
	recoveryMust(t, err)
	var count int
	recoveryMust(t, pool.QueryRow(ctx, `SELECT count(*) FROM fused_engine_idempotency_keys WHERE id=$1`, response.ID).Scan(&count))
	// Once the logical run expires, its provider response must be deleted by the same maintenance pass.
	if count != 0 {
		t.Fatal("expired provider response retained")
	}
}

// TestSharedIdempotencyResponseRetention preserves one stored response while a second run still depends on it.
func TestSharedIdempotencyResponseRetention(t *testing.T) {
	repository, pool, ctx, first, _ := newUnifiedWebhookDatabase(t)
	first.Recovery = &ExecutionRecoveryPolicy{Mode: "manual", MaxAttempts: 3, BackoffSeconds: 1}
	recoveryMust(t, repository.CreateExecutionResult(ctx, first))
	response := &models.IdempotentExecution{ID: uuid.New(), AppID: first.AppID, RunID: &first.ID, IdempotencyKeyHash: "shared-business-key", RequestBodyHash: "body", ExpiresAt: time.Now().Add(-time.Hour), ResponseBody: []byte(`{"id":9}`), ResponseStatus: 200, ResponseMediaFamily: "json"}
	recoveryMust(t, repository.ReserveIdempotentExecution(ctx, response))
	recoveryMust(t, repository.SaveIdempotentExecution(ctx, response))
	second := first
	second.ID = uuid.New()
	second.IdempotencyKeyHash = ""
	recoveryMust(t, repository.CreateExecutionResult(ctx, second))
	recoveryMust(t, repository.RetainIdempotentExecution(ctx, response.ID, second.ID))
	recoveryMust(t, repository.CompleteExecutionResult(ctx, first.AccountID, first.AppID, first.ID, "succeeded", json.RawMessage(`{}`), json.RawMessage(`null`), "", ""))
	_, err := pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '3 days',expires_at=NOW()-INTERVAL '2 days' WHERE id=$1`, first.ID)
	recoveryMust(t, err)
	_, err = repository.DeleteExpiredExecutionResults(ctx, time.Now(), 1000)
	recoveryMust(t, err)
	got, err := repository.GetIdempotentExecution(ctx, first.AppID, response.IdempotencyKeyHash, response.RequestBodyHash)
	recoveryMust(t, err)
	// Expiring the original owner must not invalidate a response already consumed by another run.
	if string(got.ResponseBody) != `{"id":9}` {
		t.Fatalf("shared response=%+v", got)
	}
	recoveryMust(t, repository.CompleteExecutionResult(ctx, second.AccountID, second.AppID, second.ID, "succeeded", json.RawMessage(`{}`), json.RawMessage(`null`), "", ""))
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '3 days',expires_at=NOW()-INTERVAL '2 days' WHERE id=$1`, second.ID)
	recoveryMust(t, err)
	_, err = repository.DeleteExpiredExecutionResults(ctx, time.Now(), 1000)
	recoveryMust(t, err)
	_, err = repository.GetIdempotentExecution(ctx, first.AppID, response.IdempotencyKeyHash, response.RequestBodyHash)
	// Once all dependent runs expire, the shared bytes and references must be removed together.
	if !errors.Is(err, ErrIdempotentExecutionNotFound) {
		t.Fatalf("expired shared response=%v", err)
	}
}
