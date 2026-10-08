package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/google/uuid"
)

// TestMCPCatalogPersistence exercises real PostgreSQL promotion, isolation, expiry, retry, and concurrent apply.
func TestMCPCatalogPersistence(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	// PostgreSQL integration tests require an explicit disposable database supplied by the test runner.
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	pool := isolatedBootstrapPool(t, ctx, dsn)
	defer pool.Close()
	repository := NewPostgresStore(pool)
	catalogs := repository.(MCPCatalogStore)
	scope := MCPCatalogScope{ServiceID: uuid.New(), VersionID: uuid.New(), SubjectID: uuid.New()}
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err := repository.AddWorkspaceServiceVersion(ctx, scope.ServiceID, "fixture", "1", scope.VersionID, "Fixture", uuid.Nil); err != nil {
		t.Fatal(err)
	}
	draft := catalogTestDraft()
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err := catalogs.SaveMCPCatalogDraft(ctx, scope, draft); err != nil {
		t.Fatal(err)
	}
	// Discovery alone must not change the saved catalog.
	if got, err := catalogs.GetMCPCatalog(ctx, scope); err != nil || got != nil {
		t.Fatalf("preview published prematurely: %+v %v", got, err)
	}
	foreign := scope
	foreign.SubjectID = uuid.New()
	// An opaque catalog ID must never bypass actor isolation.
	if _, err := catalogs.GetMCPCatalogDraft(ctx, foreign, draft.ID); !errors.Is(err, ErrMCPCatalogConflict) {
		t.Fatalf("foreign draft visible: %v", err)
	}
	saved, err := catalogs.ApplyMCPCatalogDraft(ctx, scope, draft.ID)
	// Discovery must preserve the complete catalog while leaving approval to the user.
	if err != nil || saved.ID != draft.ID {
		t.Fatalf("apply=%+v %v", saved, err)
	}
	// Fresh store instances prove the catalog survives process-local state loss.
	fresh := NewPostgresStore(pool).(MCPCatalogStore)
	if got, err := fresh.GetMCPCatalog(ctx, scope); err != nil || got.ID != draft.ID {
		t.Fatalf("reload=%+v %v", got, err)
	}
	// An opaque catalog ID must never bypass actor isolation.
	if got, err := fresh.GetMCPCatalog(ctx, foreign); err != nil || got != nil {
		t.Fatalf("foreign catalog=%+v %v", got, err)
	}
	// Retrying the current approval must not create another revision or report a conflict.
	if _, err := fresh.ApplyMCPCatalogDraft(ctx, scope, draft.ID); err != nil {
		t.Fatalf("idempotent retry=%v", err)
	}
	// Expiry never advances the head or removes the previous approved revision.
	expired := catalogTestDraft()
	expired.BaseID = &draft.ID
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err := catalogs.SaveMCPCatalogDraft(ctx, scope, expired); err != nil {
		t.Fatal(err)
	}
	// Only the still-current reviewed preview may change the saved catalog.
	if _, err := catalogs.ApplyMCPCatalogDraft(ctx, scope, expired.ID); !errors.Is(err, ErrMCPCatalogConflict) {
		t.Fatalf("expired apply=%v", err)
	}
	next := catalogTestDraft()
	next.BaseID = &draft.ID
	// Fixture setup must succeed before the behavior assertions can be trusted.
	if err := catalogs.SaveMCPCatalogDraft(ctx, scope, next); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errs := make(chan error, 2)
	// Two simultaneous applies of the same reviewed preview must both resolve to one revision.
	for range 2 {
		wait.Add(1)
		go func() { defer wait.Done(); _, err := catalogs.ApplyMCPCatalogDraft(ctx, scope, next.ID); errs <- err }()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		// Fixture setup must succeed before the behavior assertions can be trusted.
		if err != nil {
			t.Fatal(err)
		}
	}
	// Only the still-current reviewed preview may change the saved catalog.
	if _, err := catalogs.ApplyMCPCatalogDraft(ctx, scope, draft.ID); !errors.Is(err, ErrMCPCatalogConflict) {
		t.Fatalf("old draft rolled back head: %v", err)
	}
	stale := catalogTestDraft()
	stale.BaseID = &draft.ID
	// Only the still-current reviewed preview may change the saved catalog.
	if err := catalogs.SaveMCPCatalogDraft(ctx, scope, stale); !errors.Is(err, ErrMCPCatalogConflict) {
		t.Fatalf("stale discovery=%v", err)
	}
}

// catalogTestDraft supplies bounded credential-free definitions with a valid review lifetime.
func catalogTestDraft() MCPCatalogDraft {
	return MCPCatalogDraft{MCPCatalogSnapshot: MCPCatalogSnapshot{ID: uuid.New(), URL: "https://example.com/mcp", CreatedAt: time.Now(), Catalog: mcpcatalog.Catalog{ProtocolVersion: "2026-07-28", Server: mcpcatalog.ServerInfo{Name: "fixture", Version: "1"}}}, ExpiresAt: time.Now().Add(time.Minute)}
}
