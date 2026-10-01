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

// TestFamilyExecutionResultPostgres scopes historical receipts independently from the current version.
func TestFamilyExecutionResultPostgres(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	// Database tests require an explicitly configured disposable database.
	if url == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, url)
	// Failed initialization cannot masquerade as a successful isolation check.
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository := NewPostgresStore(pool)
	results := repository.(ExecutionResultStore)
	families := repository.(FamilyExecutionResultStore)
	record := ExecutionResult{ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(), AppVersion: "1.0.0", Status: "queued", Mode: "live", ReadHandleHash: strings.Repeat("a", 64), Input: json.RawMessage(`{}`)}
	defer pool.Exec(context.Background(), `DELETE FROM fused_unified_app_results WHERE id=$1`, record.ID)
	createCompletedTestExecution(t, ctx, results, record)
	loaded, err := families.GetFamilyExecutionResult(ctx, record.AccountID, record.AppFamilyID, record.ID)
	// The family read must retain the original exact version for diagnostics.
	if err != nil || loaded.AppID != record.AppID || loaded.AppVersion != record.AppVersion {
		t.Fatalf("family read=%+v error=%v", loaded, err)
	}
	for _, scope := range [][2]uuid.UUID{{uuid.New(), record.AppFamilyID}, {record.AccountID, uuid.New()}} {
		_, err := families.GetFamilyExecutionResult(ctx, scope[0], scope[1], record.ID)
		// Neither a different tenant nor family may retrieve an otherwise valid execution ID.
		if !errors.Is(err, ErrExecutionResultNotFound) {
			t.Fatalf("cross-scope read error=%v", err)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE fused_unified_app_results SET completed_at=NOW()-INTERVAL '2 days', expires_at=NOW()-INTERVAL '1 second' WHERE id=$1`, record.ID)
	// Expiry must apply to family reads just as it does to internal exact-version reads.
	if err != nil {
		t.Fatal(err)
	}
	_, err = families.GetFamilyExecutionResult(ctx, record.AccountID, record.AppFamilyID, record.ID)
	if !errors.Is(err, ErrExecutionResultNotFound) {
		t.Fatalf("expired family read error=%v", err)
	}
}
