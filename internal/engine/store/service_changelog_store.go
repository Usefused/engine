package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Usefused/engine/internal/shared/models"
)

// epochCursor is the zero value a service's changelog cursor takes before
// its first-ever poll -- matches fused_service_changelog_cursor's own
// `last_checked_at timestamptz NOT NULL DEFAULT 'epoch'` column default, so
// a brand-new row and "no row yet" behave identically to every caller.
var epochCursor = time.Unix(0, 0).UTC()

// GetServiceChangelogCursor reads one service's poll cursor. A missing row
// (a service that has never been polled, e.g. just activated) is not an
// error -- it returns the epoch, so the caller's first poll naturally
// fetches that service's entire changelog history instead of needing a
// separate "first run" branch.
func (s *postgresStore) GetServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID) (time.Time, error) {
	var lastCheckedAt time.Time
	err := s.db.QueryRow(ctx,
		`SELECT last_checked_at FROM fused_service_changelog_cursor WHERE service_id = $1`,
		serviceID,
	).Scan(&lastCheckedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return epochCursor, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("GetServiceChangelogCursor: %w", err)
	}
	return lastCheckedAt, nil
}

// UpsertServiceChangelogCursor advances (or creates) one service's cursor
// row. Callers own the correctness rule of what lastCheckedAt must be (max
// registry_created_at actually returned, never wall-clock time) -- this
// method just persists whatever it is given.
func (s *postgresStore) UpsertServiceChangelogCursor(ctx context.Context, serviceID uuid.UUID, lastCheckedAt time.Time) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO fused_service_changelog_cursor (service_id, last_checked_at, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (service_id) DO UPDATE SET
			last_checked_at = EXCLUDED.last_checked_at,
			updated_at = NOW()
	`, serviceID, lastCheckedAt)
	if err != nil {
		return fmt.Errorf("UpsertServiceChangelogCursor: %w", err)
	}
	return nil
}

// InsertServiceChangelogCacheEntries writes fetched Registry rows into the
// local cache, one INSERT per row inside a single transaction (mirrors
// UpsertSecrets' pattern for a bounded per-call batch -- this is called once
// per service per poll tick with however many rows that service produced
// since the last check, not a hot path needing a multi-row VALUES list).
// ON CONFLICT (registry_changelog_id) DO NOTHING makes a re-fetch of an
// already-cached row (e.g. after a crash between insert and cursor advance)
// a no-op instead of a duplicate or an error.
func (s *postgresStore) InsertServiceChangelogCacheEntries(ctx context.Context, entries []models.ServiceChangelogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("InsertServiceChangelogCacheEntries: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, entry := range entries {
		_, err := tx.Exec(ctx, `
			INSERT INTO fused_service_changelog_cache (
				registry_changelog_id, service_id, version, config_type,
				changelog_type, diff, registry_created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (registry_changelog_id) DO NOTHING
		`, entry.ID, entry.ServiceID, entry.Version, entry.ConfigType,
			entry.ChangelogType, entry.Diff, entry.CreatedAt)
		if err != nil {
			return fmt.Errorf("InsertServiceChangelogCacheEntries: insert %s: %w", entry.ID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("InsertServiceChangelogCacheEntries: commit: %w", err)
	}
	return nil
}
