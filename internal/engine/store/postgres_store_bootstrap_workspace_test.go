package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Usefused/engine/internal/shared/db"
)

func TestBootstrapWorkspaceConcurrentDifferentAccountsKeepsWinningOwner(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool)
	accounts := []uuid.UUID{uuid.New(), uuid.New()}
	type result struct {
		workspaceID uuid.UUID
		err         error
	}
	results := make(chan result, len(accounts))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index, accountID := range accounts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			workspaceID, bootstrapErr := repository.BootstrapWorkspace(ctx, accountID, "Concurrent "+accounts[index].String())
			results <- result{workspaceID: workspaceID, err: bootstrapErr}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	successes, ownerMismatches := 0, 0
	for outcome := range results {
		switch {
		case outcome.err == nil && outcome.workspaceID != uuid.Nil:
			successes++
		case errors.Is(outcome.err, ErrWorkspaceOwnerMismatch):
			ownerMismatches++
		default:
			t.Fatalf("unexpected bootstrap result: id=%s err=%v", outcome.workspaceID, outcome.err)
		}
	}
	if successes != 1 || ownerMismatches != 1 {
		t.Fatalf("successes/owner mismatches = %d/%d, want 1/1", successes, ownerMismatches)
	}
}

func TestBootstrapWorkspaceOwnerMismatchDoesNotCreateDefaultBucket(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool)
	ownerID := uuid.New()
	if _, err := repository.BootstrapWorkspace(ctx, ownerID, "Owned workspace"); err != nil {
		t.Fatalf("bootstrap owner workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM fused_buckets WHERE is_default = true`); err != nil {
		t.Fatalf("remove default bucket fixture: %v", err)
	}
	if _, err := repository.BootstrapWorkspace(ctx, uuid.New(), "Wrong owner"); !errors.Is(err, ErrWorkspaceOwnerMismatch) {
		t.Fatalf("mismatched owner error = %v, want ErrWorkspaceOwnerMismatch", err)
	}
	var buckets int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_buckets`).Scan(&buckets); err != nil {
		t.Fatalf("count buckets after denied bootstrap: %v", err)
	}
	if buckets != 0 {
		t.Fatalf("denied bootstrap created %d bucket(s)", buckets)
	}
}

func isolatedBootstrapPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect bootstrap test database: %v", err)
	}
	t.Cleanup(admin.Close)
	schema := "engine_bootstrap_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create isolated bootstrap schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop isolated bootstrap schema: %v", err)
		}
	})
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Scheme == "" {
		t.Fatal("DATABASE_URL must be a PostgreSQL URL")
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	pool, err := db.InitEnginePostgres(ctx, parsed.String())
	if err != nil {
		t.Fatalf("initialize isolated bootstrap schema: %v", err)
	}
	return pool
}
