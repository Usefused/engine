package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const unifiedSourceMigrationBatchSize = 100
const unifiedSourceMigrationMaxBytes = 256 * 1024

const unifiedSourceFormatVersion = 2

// UnifiedAppSourceMigrationResult exposes only aggregate startup progress, never private app source.
type UnifiedAppSourceMigrationResult struct {
	MigratedRows int
	AlreadyOwned int
	SkippedRows  int
}

// UnifiedAppSourceMigrationStore is the startup capability for upgrading saved Unified App authoring.
type UnifiedAppSourceMigrationStore interface {
	MigrateUnifiedAppSources(context.Context) (UnifiedAppSourceMigrationResult, error)
}

type unifiedSourceMigrationRow struct {
	id      uuid.UUID
	desired []byte
}

// MigrateUnifiedAppSources upgrades every older authoring format without changing compiled versions or receipt identities.
func (s *postgresStore) MigrateUnifiedAppSources(ctx context.Context) (UnifiedAppSourceMigrationResult, error) {
	result := UnifiedAppSourceMigrationResult{}
	cursor := uuid.Nil
	// Each page commits independently so startup never holds locks across the full app history.
	for {
		batch, err := s.migrateUnifiedAppSourceBatch(ctx, cursor, &result)
		// A failed page stops startup before private source can be served inconsistently.
		if err != nil {
			return result, err
		}
		// The UUID cursor bounds each transaction and prevents a malformed row from trapping startup.
		if len(batch) == 0 {
			return result, nil
		}
		cursor = batch[len(batch)-1].id
	}
}

// migrateUnifiedAppSourceBatch commits one bounded page and reports only source changes whose version markers committed with it.
func (s *postgresStore) migrateUnifiedAppSourceBatch(ctx context.Context, after uuid.UUID, result *UnifiedAppSourceMigrationResult) ([]unifiedSourceMigrationRow, error) {
	tx, err := s.db.Begin(ctx)
	// Source bytes and their format marker must share one atomic transaction.
	if err != nil {
		return nil, fmt.Errorf("begin Unified App source migration: %w", err)
	}
	defer tx.Rollback(ctx)
	batch, err := readUnifiedSourceMigrationRows(ctx, tx, after)
	// A partial or failed read must not mark any app as migrated.
	if err != nil {
		return nil, err
	}
	page := UnifiedAppSourceMigrationResult{}
	// Each row changes only its authoring representation; immutable runtime and receipt fields remain outside the update.
	for _, row := range batch {
		if err := migrateUnifiedSourceRow(ctx, tx, row, &page); err != nil {
			return nil, err
		}
	}
	// Failed commit must never contribute unpersisted conversions to startup progress.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit Unified App source migration: %w", err)
	}
	result.MigratedRows += page.MigratedRows
	result.AlreadyOwned += page.AlreadyOwned
	result.SkippedRows += page.SkippedRows
	return batch, nil
}

// readUnifiedSourceMigrationRows locks one indexed page, including exports marked by the original timestamp-only migration.
func readUnifiedSourceMigrationRows(ctx context.Context, tx pgx.Tx, after uuid.UUID) ([]unifiedSourceMigrationRow, error) {
	rows, err := tx.Query(ctx, `SELECT id,desired_state FROM fused_config_states WHERE config_type='unified_app' AND unified_source_format_version < 2 AND id > $1 ORDER BY id LIMIT $2 FOR UPDATE`, after, unifiedSourceMigrationBatchSize)
	// A read error leaves the page available for a later startup retry.
	if err != nil {
		return nil, fmt.Errorf("read Unified App migration batch: %w", err)
	}
	defer rows.Close()
	batch := make([]unifiedSourceMigrationRow, 0, unifiedSourceMigrationBatchSize)
	// Materialize and lock the complete page before changing any source.
	for rows.Next() {
		var row unifiedSourceMigrationRow
		// An incomplete row cannot be safely converted or marked.
		if err := rows.Scan(&row.id, &row.desired); err != nil {
			return nil, fmt.Errorf("scan Unified App migration batch: %w", err)
		}
		batch = append(batch, row)
	}
	// Iterator failure must roll back the entire page rather than a successfully read prefix.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read Unified App migration batch: %w", err)
	}
	return batch, nil
}

// migrateUnifiedSourceRow records only completed conversions; unsupported source remains pending without a compatibility interpretation.
func migrateUnifiedSourceRow(ctx context.Context, tx pgx.Tx, row unifiedSourceMigrationRow, page *UnifiedAppSourceMigrationResult) error {
	source, status := migratedUnifiedAppSource(row.desired)
	// Precompiled-only or unconvertible source must not receive a misleading completion marker.
	if status == unifiedSourceSkip {
		page.SkippedRows++
		return nil
	}
	// Canonical source is marked without rewriting any bytes or historical timestamps.
	if status == unifiedSourceAlreadyOwned {
		if _, err := tx.Exec(ctx, `UPDATE fused_config_states SET unified_source_migrated_at=COALESCE(unified_source_migrated_at,NOW()),unified_source_format_version=2 WHERE id=$1`, row.id); err != nil {
			return fmt.Errorf("mark Unified App source migration: %w", err)
		}
		page.AlreadyOwned++
		return nil
	}
	// This update deliberately excludes hashes, generations, app versions, compiled bundles, tokens and traffic pointers.
	if _, err := tx.Exec(ctx, `UPDATE fused_config_states SET desired_state=jsonb_set(desired_state,'{source}',to_jsonb($2::text)),unified_source_migrated_at=COALESCE(unified_source_migrated_at,NOW()),unified_source_format_version=2 WHERE id=$1`, row.id, source); err != nil {
		return fmt.Errorf("write Unified App source migration: %w", err)
	}
	page.MigratedRows++
	return nil
}

type unifiedSourceMigrationStatus uint8

const (
	unifiedSourceSkip unifiedSourceMigrationStatus = iota
	unifiedSourceAlreadyOwned
	unifiedSourceMigrated
)

// migratedUnifiedAppSource upgrades YAML-derived metadata and retired exports into one current inline config property.
func migratedUnifiedAppSource(desired []byte) (string, unifiedSourceMigrationStatus) {
	config, source, ok := unifiedSourceMigrationInput(desired)
	// Missing source and unrelated app kinds have no editable TypeScript to convert.
	if !ok {
		return "", unifiedSourceSkip
	}
	// Already current source stays byte-for-byte intact while receiving the new format marker.
	if !hasRetiredUnifiedMetadata(source) && hasInlineUnifiedConfig(source) {
		return "", unifiedSourceAlreadyOwned
	}
	metadata, err := json.MarshalIndent(config, "", "  ")
	// Saved metadata remains authoritative; serialization must finish before any source is changed.
	if err != nil {
		return "", unifiedSourceSkip
	}
	converted, err := inlineUnifiedSourceMetadata(source, metadata)
	// Unavailable or unsafe source remains unmarked and cannot be admitted by the new authoring readers.
	if err != nil || len(converted) > unifiedSourceMigrationMaxBytes {
		return "", unifiedSourceSkip
	}
	return converted, unifiedSourceMigrated
}

// unifiedSourceMigrationInput isolates portable persisted settings from transport fields before source rewriting.
func unifiedSourceMigrationInput(desired []byte) (map[string]json.RawMessage, string, bool) {
	var config map[string]json.RawMessage
	// Malformed state must never be guessed into deployable metadata.
	if err := json.Unmarshal(desired, &config); err != nil || config == nil {
		return nil, "", false
	}
	var kind, source string
	// Only stored Unified Apps participate in this source-format migration.
	if err := json.Unmarshal(config["kind"], &kind); err != nil || kind != "unified_app" {
		return nil, "", false
	}
	// Precompiled-only versions have no recoverable source and keep their existing runtime unchanged.
	if err := json.Unmarshal(config["source"], &source); err != nil || strings.TrimSpace(source) == "" {
		return nil, "", false
	}
	delete(config, "source")
	delete(config, "source_path")
	delete(config, "bundle_digest")
	delete(config, "type")
	return config, source, true
}
