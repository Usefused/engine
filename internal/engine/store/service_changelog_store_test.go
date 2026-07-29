package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestServiceChangelogStore groups the DB-backed integration tests for
// Phase 2's cursor + cache tables. Skipped when DATABASE_URL is unset, same
// convention as TestWorkspaceWebhookStore.
//
//	DATABASE_URL=postgres://... go test ./internal/engine/store/...
func TestServiceChangelogStore(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping service changelog store tests: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	serviceID := uuid.New()

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM fused_service_changelog_cursor WHERE service_id = $1", serviceID)
		_, _ = pool.Exec(ctx, "DELETE FROM fused_service_changelog_cache WHERE service_id = $1", serviceID)
	})

	t.Run("GetServiceChangelogCursor defaults to epoch when no row exists", func(t *testing.T) {
		got, err := s.GetServiceChangelogCursor(ctx, serviceID)
		if err != nil {
			t.Fatalf("GetServiceChangelogCursor: %v", err)
		}
		if !got.Equal(epochCursor) {
			t.Fatalf("expected epoch cursor for a never-polled service, got %v", got)
		}
	})

	t.Run("UpsertServiceChangelogCursor creates then advances", func(t *testing.T) {
		first := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
		if err := s.UpsertServiceChangelogCursor(ctx, serviceID, first); err != nil {
			t.Fatalf("UpsertServiceChangelogCursor (create): %v", err)
		}
		got, err := s.GetServiceChangelogCursor(ctx, serviceID)
		if err != nil {
			t.Fatalf("GetServiceChangelogCursor: %v", err)
		}
		if !got.Equal(first) {
			t.Fatalf("expected cursor=%v after first upsert, got %v", first, got)
		}

		second := time.Date(2026, 7, 25, 0, 0, 0, 0, time.UTC)
		if err := s.UpsertServiceChangelogCursor(ctx, serviceID, second); err != nil {
			t.Fatalf("UpsertServiceChangelogCursor (advance): %v", err)
		}
		got, err = s.GetServiceChangelogCursor(ctx, serviceID)
		if err != nil {
			t.Fatalf("GetServiceChangelogCursor: %v", err)
		}
		if !got.Equal(second) {
			t.Fatalf("expected cursor advanced to %v, got %v", second, got)
		}
	})

	t.Run("InsertServiceChangelogCacheEntries is idempotent on registry_changelog_id", func(t *testing.T) {
		entry := models.ServiceChangelogEntry{
			ID:            uuid.New(),
			ServiceID:     serviceID,
			ConfigType:    models.ServiceChangelogConfigTypeVersion,
			ChangelogType: models.ServiceChangelogTypeNew,
			CreatedAt:     time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC),
		}
		if err := s.InsertServiceChangelogCacheEntries(ctx, []models.ServiceChangelogEntry{entry}); err != nil {
			t.Fatalf("InsertServiceChangelogCacheEntries (first insert): %v", err)
		}
		// Re-inserting the exact same row (e.g. a re-fetch after a crash
		// between insert and cursor advance) must be a no-op, not an error
		// or a duplicate.
		if err := s.InsertServiceChangelogCacheEntries(ctx, []models.ServiceChangelogEntry{entry}); err != nil {
			t.Fatalf("InsertServiceChangelogCacheEntries (duplicate re-insert): %v", err)
		}

		var count int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM fused_service_changelog_cache WHERE registry_changelog_id = $1", entry.ID).Scan(&count); err != nil {
			t.Fatalf("count cache rows: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 cache row after a duplicate re-insert, got %d", count)
		}
	})
}
