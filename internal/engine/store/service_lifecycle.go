package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrServiceDeleted = errors.New("service has been deleted from the Registry")

// ServiceLifecycleStore owns monotonic receipts, deadline enforcement, and definition compaction.
type ServiceLifecycleStore interface {
	ApplyServiceLifecycle(context.Context, uuid.UUID, string, *time.Time, bool, time.Time) error
	IsServiceDeleted(context.Context, uuid.UUID) (bool, error)
}

// IsServiceDeleted reads durable authority on every call, including already warm SDK sessions.
func (s *postgresStore) IsServiceDeleted(ctx context.Context, id uuid.UUID) (bool, error) {
	var deleted bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM fused_service_lifecycle WHERE service_id=$1 AND (deleted_at IS NOT NULL OR delete_at<=clock_timestamp()))`, id).Scan(&deleted)
	return deleted, err
}

// ApplyServiceLifecycle rejects older receipts and atomically compacts definitions when deletion becomes effective.
func (s *postgresStore) ApplyServiceLifecycle(ctx context.Context, id uuid.UUID, name string, at *time.Time, deleted bool, observed time.Time) error {
	tx, err := s.db.Begin(ctx)
	// Partial tombstones must never coexist with executable definitions after a committed removal.
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Snapshot admission uses this same lock so an in-flight refresh cannot repopulate content after deletion.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text,731))`, id.String()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO fused_service_lifecycle(service_id,service_name,delete_at,deleted_at,observed_at)
 VALUES($1,$2,$3,CASE WHEN $4 THEN $5::timestamptz END,$5)
 ON CONFLICT(service_id) DO UPDATE SET service_name=COALESCE(NULLIF(EXCLUDED.service_name,''),fused_service_lifecycle.service_name),
 delete_at=EXCLUDED.delete_at,deleted_at=EXCLUDED.deleted_at,observed_at=EXCLUDED.observed_at
 WHERE fused_service_lifecycle.deleted_at IS NULL AND fused_service_lifecycle.observed_at<=EXCLUDED.observed_at`, id, name, at, deleted, observed)
	// A retry or stale cancellation must not reverse a committed deleted state.
	if err != nil {
		return err
	}
	var removed bool
	err = tx.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM fused_service_lifecycle WHERE service_id=$1`, id).Scan(&removed)
	// State inspection errors leave both the receipt and the definitions untouched.
	if err != nil {
		return err
	}
	// Only Registry-confirmed removal purges durable content: a missed cancellation must remain recoverable after reconnect.
	if removed {
		if err = purgeLocalServiceDefinition(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// purgeLocalServiceDefinition copies only identities before deleting every locally admitted contract payload.
func purgeLocalServiceDefinition(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	statements := []string{
		`UPDATE fused_service_lifecycle SET service_name=COALESCE(NULLIF(service_name,''),(SELECT service_name FROM fused_workspace_services WHERE service_id=$1),'') WHERE service_id=$1`,
		`INSERT INTO fused_deleted_service_versions(service_id,id,name) SELECT service_id,service_version_id,version FROM fused_service_contract_snapshots WHERE service_id=$1 ON CONFLICT(id) DO NOTHING`,
		`INSERT INTO fused_deleted_service_operations(service_id,id,name) SELECT snapshot.service_id,endpoint.endpoint_id,endpoint.name FROM fused_service_contract_endpoints endpoint JOIN fused_service_contract_snapshots snapshot ON snapshot.id=endpoint.snapshot_id WHERE snapshot.service_id=$1 ON CONFLICT(service_id,id) DO NOTHING`,
		`DELETE FROM fused_service_contract_snapshots WHERE service_id=$1`,
		`DELETE FROM fused_workspace_connection_profiles WHERE service_id=$1`,
		`DELETE FROM fused_workspace_execution_policies WHERE service_id=$1`,
		`DELETE FROM fused_workspace_services WHERE service_id=$1`,
		`UPDATE fused_service_changelog_cache SET diff=NULL WHERE service_id=$1`,
		`UPDATE fused_service_lifecycle SET deleted_at=COALESCE(deleted_at,delete_at,clock_timestamp()),delete_at=NULL WHERE service_id=$1`,
	}
	for _, statement := range statements {
		// Any failed removal rolls back the names and all earlier payload removals together.
		if _, err := tx.Exec(ctx, statement, id); err != nil {
			return err
		}
	}
	return nil
}

// ApplyServiceLifecycle delegates durable authority before invalidating local and peer runtime configurations.
func (s *cachedStore) ApplyServiceLifecycle(ctx context.Context, id uuid.UUID, name string, at *time.Time, deleted bool, observed time.Time) error {
	delegate, ok := s.Store.(ServiceLifecycleStore)
	// Production adapters must explicitly support compaction rather than silently dropping a removal.
	if !ok {
		return errors.New("service lifecycle store unavailable")
	}
	err := delegate.ApplyServiceLifecycle(ctx, id, name, at, deleted, observed)
	// A rejected transition must not advertise success to other runtime consumers.
	if err == nil {
		s.cache.Clear()
		s.invalidateRuntimeConfiguration(ctx)
	}
	return err
}

// IsServiceDeleted intentionally bypasses the runtime cache so warm sessions respect deletion deadlines.
func (s *cachedStore) IsServiceDeleted(ctx context.Context, id uuid.UUID) (bool, error) {
	delegate, ok := s.Store.(ServiceLifecycleStore)
	// Missing lifecycle support cannot safely authorize an execution.
	if !ok {
		return false, errors.New("service lifecycle store unavailable")
	}
	return delegate.IsServiceDeleted(ctx, id)
}
