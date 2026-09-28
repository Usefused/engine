package store

import (
	"bytes"
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

// TestReplayEvidenceEncryption keeps provider results out of plaintext persistence fields.
func TestReplayEvidenceEncryption(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	history := json.RawMessage(`{"calls":[{"result":{"customerId":"cus_123"}}]}`)
	wrapped, ciphertext, err := sealReplayEvidence(history, key)
	if err != nil {
		t.Fatalf("seal history: %v", err)
	}
	if strings.Contains(ciphertext, "cus_123") || strings.Contains(wrapped, "cus_123") {
		t.Fatal("stored evidence contains plaintext provider response")
	}
	opened, err := openReplayEvidence(wrapped, ciphertext, key)
	if err != nil || !bytes.Equal(opened, history) {
		t.Fatalf("open history: %q, %v", opened, err)
	}
	if _, err := openReplayEvidence(wrapped, ciphertext, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("wrong Engine key must not decrypt history")
	}
}

// TestReplayEvidencePostgresScope verifies exact tenant scope and expiry on encrypted histories.
func TestReplayEvidencePostgresScope(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
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
	record := ExecutionResult{
		ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(),
		AppVersion: "1", Status: "queued", Mode: "live",
		ReadHandleHash: strings.Repeat("a", 64), Input: json.RawMessage(`{"email":"a@example.com"}`),
	}
	defer pool.Exec(context.Background(), `DELETE FROM fused_execution_app_results WHERE id=$1`, record.ID)
	repository := NewPostgresStore(pool)
	createCompletedTestExecution(t, ctx, repository.(ExecutionResultStore), record)
	replay := repository.(ExecutionReplayEvidenceStore)
	key := bytes.Repeat([]byte{7}, 32)
	history := json.RawMessage(`{"calls":[{"result":{"id":"cus_123"}}]}`)
	if err := replay.SaveReplayEvidence(ctx, record.AccountID, record.AppID, record.ID, history, key); err != nil {
		t.Fatalf("save replay evidence: %v", err)
	}
	assertReplayEvidenceScope(t, ctx, replay, record, key, history)
	if _, err := pool.Exec(ctx, `UPDATE fused_execution_app_replay_evidence SET expires_at=NOW()-INTERVAL '1 second' WHERE execution_id=$1`, record.ID); err != nil {
		t.Fatalf("expire replay evidence: %v", err)
	}
	if _, err := replay.GetReplayEvidence(ctx, record.AccountID, record.AppID, record.ID, key); !errors.Is(err, ErrReplayEvidenceNotFound) {
		t.Fatalf("expected expired evidence to be unavailable, got %v", err)
	}
}

// assertReplayEvidenceScope checks that a neighboring account cannot read the encrypted history.
func assertReplayEvidenceScope(t *testing.T, ctx context.Context, replay ExecutionReplayEvidenceStore, record ExecutionResult, key []byte, history json.RawMessage) {
	t.Helper()
	loaded, err := replay.GetReplayEvidence(ctx, record.AccountID, record.AppID, record.ID, key)
	if err != nil || !bytes.Equal(loaded, history) {
		t.Fatalf("expected retained history, got %q error=%v", loaded, err)
	}
	if _, err := replay.GetReplayEvidence(ctx, uuid.New(), record.AppID, record.ID, key); !errors.Is(err, ErrReplayEvidenceNotFound) {
		t.Fatalf("expected cross-account isolation, got %v", err)
	}
}
