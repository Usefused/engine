package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ArchiveAppFamily retires an empty family, revokes its executable state, and releases its canonical name.
func (s *postgresStore) ArchiveAppFamily(ctx context.Context, accountID, appFamilyID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app_family.archive")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", appFamilyID.String()))

	tx, err := s.db.Begin(ctx)
	// No destructive step may run outside the transaction that owns the family lock.
	if err != nil {
		return fmt.Errorf("archive app family: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Any cleanup failure preserves the live family and every executable binding.
	if err := archiveAppFamilyTx(ctx, tx, accountID, appFamilyID); err != nil {
		span.RecordError(err)
		return err
	}
	// Commit is the only point at which the canonical name becomes reusable.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("archive app family: commit: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", "archived"))
	return nil
}

// archiveAppFamilyTx serializes final-version checks with name release and credential revocation.
func archiveAppFamilyTx(ctx context.Context, tx pgx.Tx, accountID, appFamilyID uuid.UUID) error {
	// The live identity lock orders this deletion against concurrent publication.
	if err := lockLiveAppFamilyForArchive(ctx, tx, accountID, appFamilyID); err != nil {
		return err
	}
	var hasVersions bool
	// Storage uncertainty cannot establish that destructive family cleanup is safe.
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fused_apps WHERE app_family_id = $1)`, appFamilyID).Scan(&hasVersions); err != nil {
		return fmt.Errorf("archive app family: inspect versions: %w", err)
	}
	// Version deactivation must remain explicit so every immutable version receives its tombstone and runtime cleanup.
	if hasVersions {
		return ErrAppFamilyNotEmpty
	}
	// Token history survives, but no active credential may outlive its logical app.
	if err := revokeArchivedFamilyTokens(ctx, tx, appFamilyID); err != nil {
		return err
	}
	// Bucket bindings are executable configuration and must not make archived history block bucket deletion.
	if _, err := tx.Exec(ctx, `DELETE FROM fused_app_family_buckets WHERE app_family_id = $1`, appFamilyID); err != nil {
		return fmt.Errorf("archive app family: remove bucket binding: %w", err)
	}
	bindingTag, err := tx.Exec(ctx, `DELETE FROM fused_role_bindings WHERE resource_type = 'app' AND resource_id = $1`, appFamilyID)
	// Grants belong to the deleted live boundary and must never transfer to a same-name replacement.
	if err != nil {
		return fmt.Errorf("archive app family: remove role bindings: %w", err)
	}
	actor, _ := accesscontrol.ActorFromContext(ctx)
	result, err := tx.Exec(ctx, `
		UPDATE fused_app_families
		SET archived_at = clock_timestamp(),
		    archived_by_subject_id = NULLIF($2, '00000000-0000-0000-0000-000000000000'::uuid),
		    mcp_stable_app_id = NULL,
		    updated_at = clock_timestamp()
		WHERE app_family_id = $1 AND archived_at IS NULL
	`, appFamilyID, actor.SubjectID)
	// A failed identity transition must roll back all preceding credential and grant cleanup.
	if err != nil {
		return fmt.Errorf("archive app family: retain identity: %w", err)
	}
	// The locked live row must transition exactly once; a missing update indicates lifecycle drift.
	if result.RowsAffected() != 1 {
		return ErrAppFamilyNotFound
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, bindingTag.RowsAffected() > 0)
	// Authorization caches must observe removed grants before later requests use the released name.
	if err != nil {
		return err
	}
	return auditAppFamilyArchive(ctx, tx, appFamilyID, revision)
}

// lockLiveAppFamilyForArchive prevents a concurrent publication from racing the empty-family check.
func lockLiveAppFamilyForArchive(ctx context.Context, tx pgx.Tx, accountID, appFamilyID uuid.UUID) error {
	var found uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT app_family_id
		FROM fused_app_families
		WHERE app_family_id = $1 AND account_id = $2 AND archived_at IS NULL
		FOR UPDATE
	`, appFamilyID, accountID).Scan(&found)
	// Cross-account and already-archived identities are both intentionally undiscoverable.
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppFamilyNotFound
	}
	// Database failures remain distinct from an intentionally hidden identity.
	if err != nil {
		return fmt.Errorf("archive app family: lock: %w", err)
	}
	return nil
}

// revokeArchivedFamilyTokens removes every executable hash while retaining credential-free issuance history.
func revokeArchivedFamilyTokens(ctx context.Context, tx pgx.Tx, appFamilyID uuid.UUID) error {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	_, err := tx.Exec(ctx, `
		WITH targets AS (
			SELECT id FROM fused_app_tokens WHERE app_family_id = $1 FOR UPDATE
		), retained AS (
			UPDATE fused_app_token_history history
			SET status = 'revoked', terminated_at = clock_timestamp(),
			    termination_reason = 'revoked', terminated_by_subject_id = $2,
			    terminated_by_credential_id = $3
			FROM targets WHERE history.id = targets.id
		), removed AS (
			DELETE FROM fused_app_tokens active USING targets
			WHERE active.id = targets.id RETURNING active.id
		)
		SELECT COUNT(*) FROM removed
	`, appFamilyID, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID))
	// Token history and active-hash removal are atomic with the enclosing family transaction.
	if err != nil {
		return fmt.Errorf("archive app family: revoke tokens: %w", err)
	}
	return nil
}

// auditAppFamilyArchive records the destructive user action without app configuration or credential material.
func auditAppFamilyArchive(ctx context.Context, tx pgx.Tx, appFamilyID uuid.UUID, revision int64) error {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	permission, err := appPermissionForAudit(ctx, tx, appFamilyID, accesscontrol.PermissionAppManage)
	// Audit must identify the actual app type before persisting the mutation.
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, trace_id, outcome, metadata)
		VALUES ($1, $2, 'app.family.archive', $3, 'app', $4, $5, 'succeeded',
			jsonb_build_object('authorization_revision', $6::bigint, 'changed', true))
	`, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID), permission,
		appFamilyID, trace.SpanFromContext(ctx).SpanContext().TraceID().String(), revision)
	// Missing audit evidence blocks the family mutation from committing.
	if err != nil {
		return fmt.Errorf("audit app family archive: %w", err)
	}
	return nil
}
