package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// WorkspaceServiceIdentityWriter refreshes verified metadata without changing membership or version ownership.
type WorkspaceServiceIdentityWriter interface {
	RefreshWorkspaceServiceIdentity(context.Context, uuid.UUID, uuid.UUID, string, string) error
}

// RefreshWorkspaceServiceIdentity repairs legacy references only while the exact version remains active.
func (s *postgresStore) RefreshWorkspaceServiceIdentity(ctx context.Context, serviceID, versionID uuid.UUID, reference, name string) error {
	result, err := s.db.Exec(ctx, `
		UPDATE fused_workspace_services AS service
		-- Incomplete metadata must not erase a previously verified reference or name.
		SET service_slug = COALESCE(NULLIF($3, ''), service.service_slug),
		    service_name = COALESCE(NULLIF($4, ''), service.service_name)
		WHERE service.service_id = $1
		  -- A concurrent removal or deprecation must never be reversed by an identity refresh.
		  AND EXISTS (SELECT 1 FROM fused_workspace_service_versions AS version
		              WHERE version.service_id = service.service_id
		                AND version.service_version_id = $2 AND version.status <> 'deprecated')
	`, serviceID, versionID, reference, name)
	var postgresError *pgconn.PgError
	// Preserve the competing owner's identity and expose the established conflict classification.
	if errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "uq_fused_workspace_services_slug_ci" {
		return fmt.Errorf("%w: %w", ErrWorkspaceServiceSlugConflict, err)
	}
	// Database failures cannot be mistaken for an already-correct identity.
	if err != nil {
		return fmt.Errorf("RefreshWorkspaceServiceIdentity: %w", err)
	}
	// The caller's activation check is advisory; the write itself must still find the exact active tuple.
	if result.RowsAffected() == 0 {
		return ErrWorkspaceServiceVersionNotFound
	}
	return nil
}

// RefreshWorkspaceServiceIdentity forwards verified metadata through the production cache wrapper; references are not cached.
func (s *cachedStore) RefreshWorkspaceServiceIdentity(ctx context.Context, serviceID, versionID uuid.UUID, reference, name string) error {
	writer, ok := s.Store.(WorkspaceServiceIdentityWriter)
	// Missing durable support must not turn a requested repair into a successful no-op.
	if !ok {
		return errors.New("workspace service identity writer is unavailable")
	}
	return writer.RefreshWorkspaceServiceIdentity(ctx, serviceID, versionID, reference, name)
}
