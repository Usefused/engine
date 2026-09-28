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

// TestExecutionResultDataBound checks canonical byte limits before JSONB persistence.
func TestExecutionResultDataBound(t *testing.T) {
	accepted := json.RawMessage(`{"value":"` + strings.Repeat("a", MaxExecutionDataBytes-12) + `"}`)
	if _, err := canonicalExecutionJSON(accepted, true); err != nil {
		t.Fatalf("expected bounded document: %v", err)
	}
	rejected := json.RawMessage(`{"value":"` + strings.Repeat("a", MaxExecutionDataBytes) + `"}`)
	if _, err := canonicalExecutionJSON(rejected, true); !errors.Is(err, ErrExecutionResultDataTooLarge) {
		t.Fatalf("expected size rejection, got %v", err)
	}
	if _, err := canonicalExecutionJSON(json.RawMessage(`{"value":`), true); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("expected JSON rejection, got %v", err)
	}
}

// TestExecutionResultSearchPolicy validates admitted paths and parameterized predicates.
func TestExecutionResultSearchPolicy(t *testing.T) {
	for _, path := range []string{"customerId", "customer.id", "a.b.c.d"} {
		if !ValidExecutionDataPath(path) {
			t.Fatalf("expected valid path %q", path)
		}
	}
	for _, path := range []string{"", "a..b", "a.b.c.d.e", "a-b", "a;DROP TABLE x"} {
		if ValidExecutionDataPath(path) {
			t.Fatalf("expected invalid path %q", path)
		}
	}
	filter := ExecutionResultSearch{AccountID: uuid.New(), AppID: uuid.New(), Limit: 20, AllowedDataPaths: []string{"customer.id"}}
	allowed, err := validateExecutionSearchFilter(filter)
	if err != nil {
		t.Fatalf("expected valid filter: %v", err)
	}
	clause, value, err := executionSearchTerm("data.customer.id", json.RawMessage(`"cus_123"`), allowed, 4)
	if err != nil || clause != "data @> $4::jsonb" || string(value.([]byte)) != `{"customer":{"id":"cus_123"}}` {
		t.Fatalf("expected parameterized containment, got clause=%q value=%q err=%v", clause, value, err)
	}
	if _, _, err := executionSearchTerm("data.secret", json.RawMessage(`"x"`), allowed, 4); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("expected undeclared data path rejection, got %v", err)
	}
}

// TestNewExecutionResultRequiresQueuedAndHashedReadHandle prevents admission with caller secrets.
func TestNewExecutionResultRequiresQueuedAndHashedReadHandle(t *testing.T) {
	record := ExecutionResult{
		ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(),
		AppTokenID: uuid.New(), AppVersion: "1", Status: "queued", Mode: "live",
		ReadHandleHash: strings.Repeat("a", 64),
	}
	if err := validateNewExecutionResult(record); err != nil {
		t.Fatalf("expected valid new record: %v", err)
	}
	record.ReadHandleHash = strings.Repeat("Z", 64)
	if err := validateNewExecutionResult(record); !errors.Is(err, ErrExecutionResultInvalid) {
		t.Fatalf("expected raw/invalid handle rejection, got %v", err)
	}
}

// TestExecutionResultPostgresLifecycle exercises bounded JSONB writes, terminal retention, tenant isolation, and admitted search.
func TestExecutionResultPostgresLifecycle(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	// The project runs database integration tests only when an isolated PostgreSQL URL is supplied.
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
	repository := NewPostgresStore(pool).(ExecutionResultStore)
	record := ExecutionResult{
		ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(),
		AppTokenID: uuid.New(), AppVersion: "1", Status: "queued", Mode: "live",
		ReadHandleHash: strings.Repeat("a", 64), Input: json.RawMessage(`{"email":"a@example.com"}`),
	}
	defer pool.Exec(context.Background(), `DELETE FROM fused_execution_app_results WHERE id=$1`, record.ID)
	createCompletedTestExecution(t, ctx, repository, record)
	assertStoredExecutionRead(t, ctx, repository, record)
	assertStoredExecutionSearch(t, ctx, repository, record)
	if err := repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "succeeded", json.RawMessage(`{"ok":false}`), json.RawMessage(`{"late":true}`), "", ""); !errors.Is(err, ErrExecutionResultTransition) {
		t.Fatalf("expected terminal rewrite rejection, got %v", err)
	}
}

// createCompletedTestExecution drives the accepted-to-terminal storage transitions.
func createCompletedTestExecution(t *testing.T, ctx context.Context, repository ExecutionResultStore, record ExecutionResult) {
	t.Helper()
	if err := repository.CreateExecutionResult(ctx, record); err != nil {
		t.Fatalf("create result: %v", err)
	}
	if err := repository.StartExecutionResult(ctx, record.AccountID, record.AppID, record.ID); err != nil {
		t.Fatalf("start result: %v", err)
	}
	pending, err := repository.GetExecutionResult(ctx, record.AccountID, record.AppID, record.ID)
	if err != nil || string(pending.Data) != "null" || pending.Status != "running" {
		t.Fatalf("intermediate data became durable before completion: %#v error=%v", pending, err)
	}
	if err := repository.CompleteExecutionResult(ctx, record.AccountID, record.AppID, record.ID, "succeeded", json.RawMessage(`{"ok":true}`), json.RawMessage(`{"customer":{"id":"cus_123"}}`), "", ""); err != nil {
		t.Fatalf("complete result: %v", err)
	}
}

// assertStoredExecutionRead verifies retention and account isolation on exact-ID reads.
func assertStoredExecutionRead(t *testing.T, ctx context.Context, repository ExecutionResultStore, record ExecutionResult) {
	t.Helper()
	loaded, err := repository.GetExecutionResult(ctx, record.AccountID, record.AppID, record.ID)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if loaded.Status != "succeeded" || loaded.CompletedAt == nil || loaded.ExpiresAt == nil {
		t.Fatalf("expected retained terminal result, got %#v error=%v", loaded, err)
	}
	if string(loaded.Data) != `{"customer": {"id": "cus_123"}}` && string(loaded.Data) != `{"customer":{"id":"cus_123"}}` {
		t.Fatalf("terminal commit omitted final document: %s", loaded.Data)
	}
	if loaded.ExpiresAt.Sub(*loaded.CompletedAt) < 24*time.Hour {
		t.Fatalf("retention is shorter than 24 hours: %#v", loaded)
	}
	if _, err := repository.GetExecutionResult(ctx, uuid.New(), record.AppID, record.ID); !errors.Is(err, ErrExecutionResultNotFound) {
		t.Fatalf("expected cross-account read isolation, got %v", err)
	}
}

// assertStoredExecutionSearch verifies indexed allowed paths and exact app scope.
func assertStoredExecutionSearch(t *testing.T, ctx context.Context, repository ExecutionResultStore, record ExecutionResult) {
	t.Helper()
	results, err := repository.SearchExecutionResults(ctx, ExecutionResultSearch{
		AccountID: record.AccountID, AppID: record.AppID, Limit: 10,
		AllowedDataPaths: []string{"customer.id"}, Where: map[string]json.RawMessage{"data.customer.id": json.RawMessage(`"cus_123"`)},
	})
	if err != nil || len(results) != 1 || results[0].ID != record.ID {
		t.Fatalf("expected indexed allowed search, got %#v error=%v", results, err)
	}
}

// TestRerunReservationPostgresDeduplicates verifies SQL uniqueness prevents duplicate live dispatch.
func TestRerunReservationPostgresDeduplicates(t *testing.T) {
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
	repository := NewPostgresStore(pool).(ExecutionResultStore)
	sourceID := uuid.New()
	run := ExecutionResult{
		ID: uuid.New(), AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: uuid.New(), AppTokenID: uuid.New(),
		AppVersion: "1", Status: "queued", Mode: "rerun",
		ReadHandleHash: strings.Repeat("a", 64), IdempotencyKeyHash: strings.Repeat("b", 64),
		Input: json.RawMessage(`{"email":"a@example.com"}`), SourceExecutionID: &sourceID,
	}
	defer pool.Exec(context.Background(), `DELETE FROM fused_execution_app_results WHERE id=$1`, run.ID)
	first, created, err := repository.CreateOrGetRerunExecutionResult(ctx, run)
	if err != nil || !created || first != run.ID {
		t.Fatalf("expected first reservation, got %s created=%v err=%v", first, created, err)
	}
	retry := run
	retry.ID = uuid.New()
	retry.ReadHandleHash = strings.Repeat("c", 64)
	second, created, err := repository.CreateOrGetRerunExecutionResult(ctx, retry)
	if err != nil || created || second != first {
		t.Fatalf("expected duplicate to return first ID, got %s created=%v err=%v", second, created, err)
	}
	retry.Input = json.RawMessage(`{"email":"different@example.com"}`)
	if _, _, err := repository.CreateOrGetRerunExecutionResult(ctx, retry); !errors.Is(err, ErrExecutionResultIdempotencyConflict) {
		t.Fatalf("expected changed-input conflict, got %v", err)
	}
}
