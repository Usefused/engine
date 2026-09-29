package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"os"
	"strings"
	"testing"
	"time"
)

// TestExecutionDiagnosticsPostgres proves encryption, write-once scope, wrong-key rejection, and retention.
func TestExecutionDiagnosticsPostgres(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	// This mutation test requires an explicitly supplied disposable database.
	if url == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, url)
	// Schema initialization must succeed before testing retained diagnostic columns.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	record := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(), AppVersion: "1.0.0", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64), Input: json.RawMessage(`{}`)}
	defer pool.Exec(context.Background(), `DELETE FROM fused_unified_app_results WHERE id=$1`, record.ID)
	repository := NewPostgresStore(pool)
	createCompletedTestExecution(t, ctx, repository.(ExecutionResultStore), record)
	diagnostics := repository.(ExecutionDiagnosticsStore)
	key := bytes.Repeat([]byte{7}, 32)
	payload := json.RawMessage(`{"calls":[],"error":{"message":"private sentinel"}}`)
	// Only a terminal result may receive its first diagnostic payload.
	if err := diagnostics.SaveExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, payload, key); err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	// Raw details must never occupy the encrypted column in plaintext.
	if err := pool.QueryRow(ctx, `SELECT diagnostic_payload FROM fused_unified_app_results WHERE id=$1`, record.ID).Scan(&ciphertext); err != nil || strings.Contains(ciphertext, "sentinel") {
		t.Fatalf("plaintext or missing diagnostic: %v", err)
	}
	opened, err := diagnostics.GetExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, key)
	// Authorized exact reads preserve useful original text without broad redaction.
	if err != nil || !bytes.Equal(opened, payload) {
		t.Fatalf("read: %s %v", opened, err)
	}
	for _, scope := range [][2]uuid.UUID{{uuid.New(), record.AppID}, {record.AccountID, uuid.New()}} {
		// Neither tenant nor version scope can be weakened by knowing an execution ID.
		if _, err := diagnostics.GetExecutionDiagnostics(ctx, scope[0], scope[1], record.ID, key); !errors.Is(err, ErrExecutionResultNotFound) {
			t.Fatalf("cross-scope read: %v", err)
		}
	}
	// Engine-key mismatch cannot yield decrypted evidence.
	if _, err := diagnostics.GetExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, bytes.Repeat([]byte{8}, 32)); err == nil {
		t.Fatal("wrong key admitted")
	}
	// A later writer cannot silently replace captured evidence.
	if err := diagnostics.SaveExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, payload, key); !errors.Is(err, ErrExecutionResultNotFound) {
		t.Fatalf("overwrite admitted: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '2 days',expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Retention is checked on read, even before the cleanup worker deletes expired rows.
	if _, err := diagnostics.GetExecutionDiagnostics(ctx, record.AccountID, record.AppID, record.ID, key); !errors.Is(err, ErrExecutionResultNotFound) {
		t.Fatalf("expired read: %v", err)
	}
}
