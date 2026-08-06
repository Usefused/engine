package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) ListWorkspaceShares(ctx context.Context, options WorkspaceShareListOptions) ([]WorkspaceShare, int, error) {
	if err := validateWorkspaceShareListOptions(options); err != nil {
		return nil, 0, err
	}
	resourceType := ""
	if options.ResourceType != nil {
		resourceType = string(*options.ResourceType)
	}
	rows, err := s.db.Query(ctx, listWorkspaceSharesSQL, resourceType, options.Limit, options.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list workspace shares: %w", err)
	}
	defer rows.Close()
	shares := make([]WorkspaceShare, 0, options.Limit)
	total := 0
	for rows.Next() {
		var shareID, resourceID *uuid.UUID
		var roleSlug, roleName, resolvedType, resourceName *string
		var createdAt *time.Time
		if err := rows.Scan(&shareID, &roleSlug, &roleName, &resolvedType, &resourceID, &resourceName, &createdAt, &total); err != nil {
			return nil, 0, fmt.Errorf("scan workspace share: %w", err)
		}
		if shareID != nil {
			shares = append(shares, WorkspaceShare{ID: *shareID, RoleSlug: *roleSlug, RoleDisplayName: *roleName,
				Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(*resolvedType), ID: *resourceID}, ResourceDisplayName: *resourceName, CreatedAt: *createdAt})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate workspace shares: %w", err)
	}
	return shares, total, nil
}

func (s *postgresStore) GrantWorkspaceShare(ctx context.Context, input WorkspaceShareMutation) (WorkspaceShareMutationResult, error) {
	return s.mutateWorkspaceShare(ctx, input, true)
}

func (s *postgresStore) RevokeWorkspaceShare(ctx context.Context, input WorkspaceShareMutation) (WorkspaceShareMutationResult, error) {
	return s.mutateWorkspaceShare(ctx, input, false)
}

func (s *postgresStore) mutateWorkspaceShare(ctx context.Context, input WorkspaceShareMutation, grant bool) (WorkspaceShareMutationResult, error) {
	operation := workspaceShareOperation(grant)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.workspace_share."+operation)
	defer span.End()
	if err := validateWorkspaceShareMutation(input); err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	tx, err := s.beginAccessMutation(ctx, input.Actor)
	if err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	share, workspaceID, roleID, resourceExists, err := resolveWorkspaceShareTarget(ctx, tx, input.Resource)
	if err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	if grant && !resourceExists {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, ErrInvalidWorkspaceShare)
	}
	changed, err := writeWorkspaceShare(ctx, tx, &share, workspaceID, roleID, input.Actor.SubjectID, grant)
	if err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	revision, err := finalizeWorkspaceShareMutation(ctx, tx, input, share.RoleSlug, operation, changed)
	if err != nil {
		return WorkspaceShareMutationResult{}, recordWorkspaceShareSpanError(span, err)
	}
	span.SetAttributes(attribute.Bool("engine.access.changed", changed), attribute.Int64("engine.authorization.revision", revision),
		attribute.String("engine.resource.type", string(input.Resource.Type)), attribute.String("engine.resource.id", input.Resource.ID.String()))
	return WorkspaceShareMutationResult{Share: share, AuthorizationRevision: revision, Changed: changed}, nil
}

func resolveWorkspaceShareTarget(ctx context.Context, tx pgx.Tx, resource accesscontrol.ResourceRef) (WorkspaceShare, uuid.UUID, uuid.UUID, bool, error) {
	roleSlug, _ := workspaceShareRole(resource.Type)
	share := WorkspaceShare{RoleSlug: roleSlug, Resource: resource}
	var workspaceID, roleID uuid.UUID
	var displayName *string
	var resourceExists bool
	// A family remains shareable while it has any runnable version. Revoke still
	// proceeds by binding identity so administrators can remove a stale share.
	err := tx.QueryRow(ctx, `
		SELECT workspace.id, role.id, role.display_name,
			CASE $2::text WHEN 'bucket' THEN bucket.name WHEN 'app' THEN app.display_name END,
			CASE $2::text WHEN 'bucket' THEN bucket.id IS NOT NULL WHEN 'app' THEN app.app_family_id IS NOT NULL ELSE false END
		FROM fused_workspaces workspace
		JOIN fused_roles role ON role.slug = $1 AND role.system_role = true AND role.scope_type = $2
		LEFT JOIN fused_buckets bucket ON $2 = 'bucket' AND bucket.id = $3
		LEFT JOIN fused_app_families app ON $2 = 'app' AND app.app_family_id = $3
			AND EXISTS (SELECT 1 FROM fused_apps version WHERE version.app_family_id = app.app_family_id AND version.status IN ('active', 'deprecated'))
		WHERE workspace.singleton_key = 1
		FOR UPDATE OF workspace
	`, roleSlug, resource.Type, resource.ID).Scan(&workspaceID, &roleID, &share.RoleDisplayName, &displayName, &resourceExists)
	if err != nil {
		return WorkspaceShare{}, uuid.Nil, uuid.Nil, false, fmt.Errorf("resolve workspace share target: %w", err)
	}
	if displayName != nil {
		share.ResourceDisplayName = *displayName
	}
	return share, workspaceID, roleID, resourceExists, nil
}

func writeWorkspaceShare(ctx context.Context, tx pgx.Tx, share *WorkspaceShare, workspaceID, roleID, actorSubjectID uuid.UUID, grant bool) (bool, error) {
	if !grant {
		return deleteWorkspaceShare(ctx, tx, share, workspaceID)
	}
	var changed bool
	err := tx.QueryRow(ctx, `
		WITH deleted AS (
			DELETE FROM fused_role_bindings
			WHERE subject_type = 'workspace' AND subject_id = $1 AND resource_type = $3 AND resource_id = $4 AND role_id <> $2
			RETURNING id
		), inserted AS (
			INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id, created_by_subject_id)
			VALUES ('workspace', $1, $2, $3, $4, $5)
			ON CONFLICT (subject_type, subject_id, role_id, resource_type, resource_id) DO NOTHING
			RETURNING id, created_at
		)
		SELECT id, created_at, true FROM inserted
		UNION ALL
		SELECT id, created_at, EXISTS (SELECT 1 FROM deleted) FROM fused_role_bindings
		WHERE subject_type = 'workspace' AND subject_id = $1 AND role_id = $2 AND resource_type = $3 AND resource_id = $4
			AND NOT EXISTS (SELECT 1 FROM inserted)
		LIMIT 1
	`, workspaceID, roleID, share.Resource.Type, share.Resource.ID, actorSubjectID).Scan(&share.ID, &share.CreatedAt, &changed)
	if err != nil {
		return false, fmt.Errorf("grant workspace share: %w", err)
	}
	return changed, nil
}

func deleteWorkspaceShare(ctx context.Context, tx pgx.Tx, share *WorkspaceShare, workspaceID uuid.UUID) (bool, error) {
	err := tx.QueryRow(ctx, `
		DELETE FROM fused_role_bindings
		WHERE subject_type = 'workspace' AND subject_id = $1 AND resource_type = $2 AND resource_id = $3
		RETURNING id, created_at
	`, workspaceID, share.Resource.Type, share.Resource.ID).Scan(&share.ID, &share.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("revoke workspace share: %w", err)
	}
	return true, nil
}

func finalizeWorkspaceShareMutation(ctx context.Context, tx pgx.Tx, input WorkspaceShareMutation, roleSlug, operation string, changed bool) (int64, error) {
	revision, err := bumpAuthorizationRevision(ctx, tx, changed)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, actor_credential_id, action, permission, resource_type,
			resource_id, request_id, trace_id, outcome, metadata)
		VALUES ($1, $2, $3, 'access.manage', $4, $5, $6, $7, 'succeeded',
			jsonb_build_object('role_slug', $8::text, 'authorization_revision', $9::bigint, 'changed', $10::boolean))
	`, input.Actor.SubjectID, input.Actor.CredentialID, "workspace.share."+operation, input.Resource.Type,
		input.Resource.ID, input.Actor.RequestID, input.Actor.TraceID, roleSlug, revision, changed)
	if err != nil {
		return 0, fmt.Errorf("audit workspace share mutation: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit workspace share %s: %w", operation, err)
	}
	return revision, nil
}

func workspaceShareOperation(grant bool) string {
	if grant {
		return "grant"
	}
	return "revoke"
}

func recordWorkspaceShareSpanError(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, "workspace share operation failed")
	return err
}

const listWorkspaceSharesSQL = `
WITH workspace AS (SELECT id FROM fused_workspaces WHERE singleton_key = 1),
filtered AS (
	SELECT binding.id, role.slug, role.display_name, binding.resource_type, binding.resource_id,
		CASE binding.resource_type WHEN 'bucket' THEN bucket.name WHEN 'app' THEN app.display_name END AS resource_name,
		binding.created_at
	FROM workspace
	JOIN fused_role_bindings binding ON binding.subject_type = 'workspace' AND binding.subject_id = workspace.id
	JOIN fused_roles role ON role.id = binding.role_id AND role.system_role = true
	LEFT JOIN fused_buckets bucket ON binding.resource_type = 'bucket' AND bucket.id = binding.resource_id
	LEFT JOIN fused_app_families app ON binding.resource_type = 'app' AND app.app_family_id = binding.resource_id
	WHERE binding.resource_type IN ('bucket', 'app') AND ($1 = '' OR binding.resource_type = $1)
), page AS (
	SELECT * FROM filtered ORDER BY resource_type, resource_name, resource_id LIMIT $2 OFFSET $3
), summary AS (SELECT COUNT(*)::int AS total FROM filtered)
SELECT page.id, page.slug, page.display_name, page.resource_type, page.resource_id, page.resource_name, page.created_at, summary.total
FROM summary LEFT JOIN page ON true ORDER BY page.resource_type, page.resource_name, page.resource_id`

var _ WorkspaceAccessRepository = (*postgresStore)(nil)
