package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const sdkGeneratorMigrationBatchSize = 100

// legacySDKGeneratorVersions lists the retired generator contracts eligible for in-place upgrade to the current version.
var legacySDKGeneratorVersions = []string{
	"registry-generator-v6",
	"registry-generator-v7",
}

// SDKGeneratorVersionMigrationResult exposes only aggregate startup progress, never app metadata or source.
type SDKGeneratorVersionMigrationResult struct {
	MigratedRows  int
	ReboundJobIDs int
}

// SDKGeneratorVersionMigrationStore is the startup capability for upgrading retired SDK generator versions.
type SDKGeneratorVersionMigrationStore interface {
	MigrateSDKGeneratorVersions(context.Context, uuid.UUID) (SDKGeneratorVersionMigrationResult, error)
}

type sdkGeneratorMigrationRow struct {
	appID              uuid.UUID
	accountID          uuid.UUID
	sourceHash         string
	sdkGenerationJobID string
}

// MigrateSDKGeneratorVersions upgrades v6 and v7 SDK rows to the current generator contract in bounded transactional pages.
func (s *postgresStore) MigrateSDKGeneratorVersions(ctx context.Context, engineID uuid.UUID) (SDKGeneratorVersionMigrationResult, error) {
	result := SDKGeneratorVersionMigrationResult{}
	cursor := uuid.Nil
	// Bounded pages avoid holding row locks across the entire app history during startup.
	for {
		batch, err := s.migrateSDKGeneratorVersionBatch(ctx, engineID, cursor, &result)
		// Any failed page must abort startup before workers replay stale generator versions against Registry.
		if err != nil {
			return result, err
		}
		// An empty page proves every eligible legacy SDK row has been upgraded.
		if len(batch) == 0 {
			return result, nil
		}
		cursor = batch[len(batch)-1].appID
	}
}

// migrateSDKGeneratorVersionBatch upgrades one locked page atomically and updates totals only after commit.
func (s *postgresStore) migrateSDKGeneratorVersionBatch(ctx context.Context, engineID, after uuid.UUID, result *SDKGeneratorVersionMigrationResult) ([]sdkGeneratorMigrationRow, error) {
	tx, err := s.db.Begin(ctx)
	// Generator version and deterministic job ID updates must commit together per page.
	if err != nil {
		return nil, fmt.Errorf("begin SDK generator version migration: %w", err)
	}
	defer tx.Rollback(ctx)
	batch, err := readSDKGeneratorMigrationRows(ctx, tx, after)
	// A failed page scan leaves rows untouched for retry on the next startup.
	if err != nil {
		return nil, err
	}
	page := SDKGeneratorVersionMigrationResult{}
	// Upgrade each locked row in place without altering selections, status, or family identity.
	for _, row := range batch {
		if err := migrateSDKGeneratorVersionRow(ctx, tx, engineID, row, &page); err != nil {
			return nil, err
		}
	}
	// Aggregate startup counts must only reflect committed database mutations.
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit SDK generator version migration: %w", err)
	}
	result.MigratedRows += page.MigratedRows
	result.ReboundJobIDs += page.ReboundJobIDs
	return batch, nil
}

// readSDKGeneratorMigrationRows locks one keyset page of v6/v7 SDK apps without touching MCP, Unified Apps, or current versions.
func readSDKGeneratorMigrationRows(ctx context.Context, tx pgx.Tx, after uuid.UUID) ([]sdkGeneratorMigrationRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT app.app_id, app.account_id, app.source_hash, COALESCE(app.sdk_generation_job_id, '')
		FROM fused_apps app
		JOIN fused_app_families family
		  ON family.app_family_id = app.app_family_id
		 AND family.account_id = app.account_id
		WHERE family.kind = 'sdk'
		  AND app.generator_version = ANY($1::text[])
		  AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR app.app_id > $2)
		ORDER BY app.app_id
		LIMIT $3
		FOR UPDATE OF app
	`, legacySDKGeneratorVersions, after, sdkGeneratorMigrationBatchSize)
	// A query error aborts the page cleanly before any mutation is attempted.
	if err != nil {
		return nil, fmt.Errorf("read SDK generator migration batch: %w", err)
	}
	defer rows.Close()
	batch := make([]sdkGeneratorMigrationRow, 0, sdkGeneratorMigrationBatchSize)
	// Materialize the complete locked page before issuing per-row updates on the same transaction.
	for rows.Next() {
		var row sdkGeneratorMigrationRow
		// Incomplete row identity cannot safely recompute a deterministic Registry job ID.
		if err := rows.Scan(&row.appID, &row.accountID, &row.sourceHash, &row.sdkGenerationJobID); err != nil {
			return nil, fmt.Errorf("scan SDK generator migration batch: %w", err)
		}
		batch = append(batch, row)
	}
	// Deferred cursor errors must roll back the batch instead of silently truncating it.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SDK generator migration batch: %w", err)
	}
	return batch, nil
}

// migrateSDKGeneratorVersionRow updates one SDK version to the current generator and rebinds its deterministic job ID when job-backed.
func migrateSDKGeneratorVersionRow(ctx context.Context, tx pgx.Tx, engineID uuid.UUID, row sdkGeneratorMigrationRow, page *SDKGeneratorVersionMigrationResult) error {
	// Direct API and jobless rows must keep sdk_generation_job_id NULL to uphold chk_fused_apps_sdk_skipped_jobless.
	if strings.TrimSpace(row.sdkGenerationJobID) == "" {
		if _, err := tx.Exec(ctx, `
			UPDATE fused_apps
			SET generator_version = $2
			WHERE app_id = $1
		`, row.appID, models.SDKGeneratorVersion); err != nil {
			return fmt.Errorf("update jobless SDK generator version: %w", err)
		}
		page.MigratedRows++
		return nil
	}
	// Job-backed SDK builds require the authenticated Registry Engine ID to recompute the exact Registry job identity.
	if engineID == uuid.Nil {
		return errors.New("migrate SDK generator version: Engine ID is required to rebind job-backed SDK builds")
	}
	reboundJobID := deterministicSDKGenerationJobID(row.accountID, engineID, row.appID, row.sourceHash, models.SDKGeneratorVersion)
	if _, err := tx.Exec(ctx, `
		UPDATE fused_apps
		SET generator_version = $2,
		    sdk_generation_job_id = $3
		WHERE app_id = $1
	`, row.appID, models.SDKGeneratorVersion, reboundJobID); err != nil {
		return fmt.Errorf("update job-backed SDK generator version: %w", err)
	}
	page.MigratedRows++
	page.ReboundJobIDs++
	return nil
}

// deterministicSDKGenerationJobID mirrors Registry's sdkGenerationIdentity so pending builds match Registry's v8 replay response.
func deterministicSDKGenerationJobID(accountID, engineID, appID uuid.UUID, sourceHash, generatorVersion string) string {
	identity := strings.Join([]string{
		accountID.String(), engineID.String(), appID.String(), sourceHash, generatorVersion,
	}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("sdk-job:"+identity)).String()
}
