package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) CreateTeam(ctx context.Context, input TeamMutation) (TeamMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team.create")
	defer span.End()
	if err := validateTeamMutation(input); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginTeamMutation(ctx, input.Actor)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	team, err := insertTeam(ctx, tx, input)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	// Team metadata alone cannot change an authorization snapshot.
	revision, err := bumpAuthorizationRevision(ctx, tx, false)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := auditTeamMutation(ctx, tx, input.Actor, "team.create", team.ID, revision, true); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit team create: %w", err))
	}
	span.SetAttributes(attribute.String("engine.team.id", team.ID.String()), attribute.Int64("engine.authorization.revision", revision))
	return TeamMutationResult{Team: team, AuthorizationRevision: revision, Changed: true}, nil
}

func insertTeam(ctx context.Context, tx pgx.Tx, input TeamMutation) (Team, error) {
	var team Team
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_teams (name, slug, description)
		VALUES ($1, $2, $3)
		RETURNING id, name, slug, description, status, created_at, updated_at
	`, input.Name, input.Slug, input.Description).Scan(
		&team.ID, &team.Name, &team.Slug, &team.Description, &team.Status, &team.CreatedAt, &team.UpdatedAt,
	)
	if isTeamSlugViolation(err) {
		return Team{}, ErrTeamSlugConflict
	}
	if err != nil {
		return Team{}, fmt.Errorf("create team: %w", err)
	}
	team.Bindings = []TeamBinding{}
	return team, nil
}

func (s *postgresStore) GetTeam(ctx context.Context, teamID uuid.UUID) (Team, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team.get")
	defer span.End()
	if teamID == uuid.Nil {
		return Team{}, ErrTeamNotFound
	}
	rows, err := s.db.Query(ctx, teamSelectSQL+` WHERE team.id = $1 ORDER BY binding.created_at, binding.id`, teamID)
	if err != nil {
		return Team{}, recordTeamSpanError(span, fmt.Errorf("get team: %w", err))
	}
	defer rows.Close()
	teams, err := scanTeams(rows)
	if err != nil {
		return Team{}, recordTeamSpanError(span, err)
	}
	if len(teams) == 0 {
		return Team{}, ErrTeamNotFound
	}
	return teams[0], nil
}

func (s *postgresStore) ListTeams(ctx context.Context, options TeamListOptions) ([]Team, int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team.list")
	defer span.End()
	statuses, err := validateTeamListOptions(&options)
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	batch := &pgx.Batch{}
	batch.Queue(teamCountSQL, statuses, options.Search)
	batch.Queue(teamListSQL, statuses, options.Search, options.Limit, options.Offset)
	results := s.db.SendBatch(ctx, batch)
	defer results.Close()
	var total int
	if err := results.QueryRow().Scan(&total); err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("count teams: %w", err))
	}
	rows, err := results.Query()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("list teams: %w", err))
	}
	teams, err := scanTeams(rows)
	rows.Close()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	span.SetAttributes(attribute.Int("engine.team.count", len(teams)))
	return teams, total, nil
}

func validateTeamListOptions(options *TeamListOptions) ([]string, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 200 || options.Offset < 0 || len(options.Search) > 100 {
		return nil, fmt.Errorf("%w: invalid team list pagination or search", ErrInvalidTeam)
	}
	statuses := make([]string, len(options.Statuses))
	for index, status := range options.Statuses {
		if err := validateTeamStatus(status); err != nil {
			return nil, err
		}
		statuses[index] = string(status)
	}
	return statuses, nil
}

func (s *postgresStore) UpdateTeam(ctx context.Context, teamID uuid.UUID, patch TeamPatch) (TeamMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team.update")
	defer span.End()
	if teamID == uuid.Nil {
		return TeamMutationResult{}, ErrTeamNotFound
	}
	if err := validateTeamPatch(patch); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginTeamMutation(ctx, patch.Actor)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureOwnerTeamAuthorized(ctx, tx, teamID, patch.Actor.SubjectID, false); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	team, changed, err := updateTeamRow(ctx, tx, teamID, patch)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	// Name, slug, and description are display metadata, not grants.
	revision, err := bumpAuthorizationRevision(ctx, tx, false)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := auditTeamMutation(ctx, tx, patch.Actor, "team.update", team.ID, revision, changed); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit team update: %w", err))
	}
	span.SetAttributes(attribute.Bool("engine.access.changed", changed), attribute.Int64("engine.authorization.revision", revision))
	return TeamMutationResult{Team: team, AuthorizationRevision: revision, Changed: changed}, nil
}

func updateTeamRow(ctx context.Context, tx pgx.Tx, teamID uuid.UUID, patch TeamPatch) (Team, bool, error) {
	query := `
		WITH updated AS (
			UPDATE fused_teams SET
				name = COALESCE($2, name), slug = COALESCE($3, slug),
				description = COALESCE($4, description), updated_at = NOW()
			WHERE id = $1 AND (name, slug, description) IS DISTINCT FROM
				(COALESCE($2, name), COALESCE($3, slug), COALESCE($4, description))
			RETURNING id, name, slug, description, status, created_at, updated_at
		)
		SELECT id, name, slug, description, status, created_at, updated_at, true FROM updated
		UNION ALL
		SELECT id, name, slug, description, status, created_at, updated_at, false
		FROM fused_teams WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM updated)
		LIMIT 1
	`
	var team Team
	var changed bool
	err := tx.QueryRow(ctx, query, teamID, patch.Name, patch.Slug, patch.Description).Scan(
		&team.ID, &team.Name, &team.Slug, &team.Description, &team.Status, &team.CreatedAt, &team.UpdatedAt, &changed,
	)
	if isTeamSlugViolation(err) {
		return Team{}, false, ErrTeamSlugConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, false, ErrTeamNotFound
	}
	if err != nil {
		return Team{}, false, fmt.Errorf("update team: %w", err)
	}
	team.Bindings = []TeamBinding{}
	return team, changed, nil
}

func (s *postgresStore) ArchiveTeam(ctx context.Context, teamID uuid.UUID, actor MutationActor) (TeamMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team.archive")
	defer span.End()
	if teamID == uuid.Nil {
		return TeamMutationResult{}, ErrTeamNotFound
	}
	tx, err := s.beginTeamMutation(ctx, actor)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	team, bindingCount, activeArtifactCount, err := lockOwnerAuthorizedTeamForArchive(ctx, tx, teamID, actor.SubjectID)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureTeamArchivable(team, bindingCount, activeArtifactCount); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	team, changed, err := archiveActiveTeam(ctx, tx, team)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	// Archiving requires zero bindings and zero owned artifacts, so it has no
	// effective grant to invalidate at the point the status changes.
	revision, err := bumpAuthorizationRevision(ctx, tx, false)
	if err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := auditTeamMutation(ctx, tx, actor, "team.archive", team.ID, revision, changed); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TeamMutationResult{}, recordTeamSpanError(span, fmt.Errorf("commit team archive: %w", err))
	}
	return TeamMutationResult{Team: team, AuthorizationRevision: revision, Changed: changed}, nil
}

func ensureTeamArchivable(team Team, bindingCount, activeArtifactCount int) error {
	if team.Status == TeamStatusActive && (bindingCount > 0 || activeArtifactCount > 0) {
		return &TeamArchiveConflictError{BindingCount: bindingCount, ActiveArtifactCount: activeArtifactCount}
	}
	return nil
}

func archiveActiveTeam(ctx context.Context, tx pgx.Tx, team Team) (Team, bool, error) {
	if team.Status == TeamStatusArchived {
		return team, false, nil
	}
	archived, err := archiveTeamRow(ctx, tx, team.ID)
	return archived, true, err
}

func lockTeamForArchive(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) (Team, int, int, error) {
	var team Team
	err := tx.QueryRow(ctx, `
		SELECT team.id, team.name, team.slug, team.description, team.status,
			team.created_at, team.updated_at
		FROM fused_teams team WHERE team.id = $1 FOR UPDATE
	`, teamID).Scan(&team.ID, &team.Name, &team.Slug, &team.Description, &team.Status, &team.CreatedAt, &team.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, 0, 0, ErrTeamNotFound
	}
	if err != nil {
		return Team{}, 0, 0, fmt.Errorf("lock team for archive: %w", err)
	}
	// Count blockers only after the row lock is acquired. Under READ COMMITTED
	// this second statement gets a fresh snapshot, so an apply that held the
	// team lock first cannot commit ownership invisibly while archive waits.
	var bindingCount, activeArtifactCount int
	err = tx.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM fused_role_bindings binding WHERE binding.subject_type = 'team' AND binding.subject_id = $1),
			(SELECT COUNT(*) FROM fused_artifact_scopes artifact WHERE artifact.owner_team_id = $1 AND artifact.deactivated_at IS NULL) +
			(SELECT COUNT(*) FROM fused_config_states state WHERE state.owner_team_id = $1 AND state.config_type = 'webhook')
	`, teamID).Scan(&bindingCount, &activeArtifactCount)
	if err != nil {
		return Team{}, 0, 0, fmt.Errorf("count team archive blockers: %w", err)
	}
	return team, bindingCount, activeArtifactCount, nil
}

func archiveTeamRow(ctx context.Context, tx pgx.Tx, teamID uuid.UUID) (Team, error) {
	var team Team
	err := tx.QueryRow(ctx, `
		UPDATE fused_teams SET status = 'archived', updated_at = NOW() WHERE id = $1
		RETURNING id, name, slug, description, status, created_at, updated_at
	`, teamID).Scan(&team.ID, &team.Name, &team.Slug, &team.Description, &team.Status, &team.CreatedAt, &team.UpdatedAt)
	if err != nil {
		return Team{}, fmt.Errorf("archive team: %w", err)
	}
	team.Bindings = []TeamBinding{}
	return team, nil
}

func (s *postgresStore) AddTeamBinding(ctx context.Context, input TeamBindingMutation) (TeamBindingMutationResult, error) {
	return s.mutateTeamBinding(ctx, input, true)
}

func (s *postgresStore) RemoveTeamBinding(ctx context.Context, input TeamBindingMutation) (TeamBindingMutationResult, error) {
	return s.mutateTeamBinding(ctx, input, false)
}

func (s *postgresStore) ClearTeamWorkspaceRole(ctx context.Context, teamID, workspaceID uuid.UUID, actor MutationActor) (TeamBindingMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_binding.clear_workspace_role")
	defer span.End()
	if teamID == uuid.Nil || workspaceID == uuid.Nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, ErrInvalidTeamBinding)
	}
	tx, err := s.beginTeamMutation(ctx, actor)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	binding, err := loadOwnerAuthorizedWorkspaceRole(ctx, tx, teamID, workspaceID, actor.SubjectID)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	changed, err := deleteTeamWorkspaceRole(ctx, tx, &binding)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureWorkspaceOwnerSafety(ctx, tx, changed); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	input := TeamBindingMutation{TeamID: teamID, RoleSlug: binding.RoleSlug, Resource: binding.Resource, Actor: actor}
	revision, err := finalizeTeamBindingMutation(ctx, tx, input, "revoke", changed)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	span.SetAttributes(attribute.Bool("engine.access.changed", changed), attribute.Int64("engine.authorization.revision", revision))
	return TeamBindingMutationResult{Binding: binding, AuthorizationRevision: revision, Changed: changed}, nil
}

func loadTeamWorkspaceRole(ctx context.Context, tx pgx.Tx, teamID, workspaceID uuid.UUID) (TeamBinding, TeamStatus, bool, error) {
	var binding TeamBinding
	var status TeamStatus
	var workspaceName *string
	var bindingID *uuid.UUID
	var roleSlug, roleDisplayName *string
	var createdAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT team.status, workspace.name, binding.id, role.slug, role.display_name, binding.created_at
		FROM fused_teams team
		LEFT JOIN fused_workspaces workspace ON workspace.id = $2
		LEFT JOIN fused_role_bindings binding ON binding.subject_type = 'team'
			AND binding.subject_id = team.id AND binding.resource_type = 'workspace' AND binding.resource_id = $2
			AND binding.role_id IN (
				SELECT id FROM fused_roles WHERE system_role = true AND scope_type = 'workspace' AND slug = ANY($3::text[])
			)
		LEFT JOIN fused_roles role ON role.id = binding.role_id AND role.system_role = true
			AND role.scope_type = 'workspace' AND role.slug = ANY($3::text[])
		WHERE team.id = $1
		LIMIT 1 FOR UPDATE OF team
	`, teamID, workspaceID, workspaceTeamRoleSlugs).Scan(&status, &workspaceName, &bindingID, &roleSlug, &roleDisplayName, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBinding{}, "", false, ErrTeamNotFound
	}
	if err != nil {
		return TeamBinding{}, "", false, fmt.Errorf("load team workspace role: %w", err)
	}
	binding.TeamID = teamID
	binding.Resource = accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}
	if workspaceName != nil {
		binding.ResourceDisplayName = *workspaceName
	}
	if bindingID != nil {
		binding.ID, binding.RoleSlug, binding.RoleDisplayName, binding.CreatedAt = *bindingID, *roleSlug, *roleDisplayName, *createdAt
	}
	return binding, status, workspaceName != nil, nil
}

func validateClearTeamWorkspaceRole(status TeamStatus, workspaceExists bool) error {
	if status == TeamStatusArchived {
		return ErrTeamArchived
	}
	if !workspaceExists {
		return ErrInvalidTeamBinding
	}
	return nil
}

func deleteTeamWorkspaceRole(ctx context.Context, tx pgx.Tx, binding *TeamBinding) (bool, error) {
	if binding.ID == uuid.Nil {
		return false, nil
	}
	tag, err := tx.Exec(ctx, `DELETE FROM fused_role_bindings WHERE id = $1`, binding.ID)
	if err != nil {
		return false, fmt.Errorf("clear team workspace role: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

var workspaceTeamRoleSlugs = []string{
	accesscontrol.RoleOwner,
	accesscontrol.RoleAdmin,
	accesscontrol.RoleBuilder,
	accesscontrol.RoleViewer,
}

func (s *postgresStore) mutateTeamBinding(ctx context.Context, input TeamBindingMutation, add bool) (TeamBindingMutationResult, error) {
	operation := teamBindingOperation(add)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_binding."+operation)
	defer span.End()
	if err := validateTeamBindingMutation(input); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginTeamMutation(ctx, input.Actor)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	binding, roleID, teamStatus, resourceExists, err := resolveTeamBindingTarget(ctx, tx, input)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := validateResolvedTeamBinding(add, teamStatus, resourceExists); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureOwnerTeamAuthorized(ctx, tx, input.TeamID, input.Actor.SubjectID, ownerRoleRequested(input.Resource, input.RoleSlug)); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	changed, err := writeTeamBinding(ctx, tx, &binding, roleID, input.Actor.SubjectID, add)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureTeamBindingOwnerSafety(ctx, tx, input.Resource.Type, changed); err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	revision, err := finalizeTeamBindingMutation(ctx, tx, input, operation, changed)
	if err != nil {
		return TeamBindingMutationResult{}, recordTeamSpanError(span, err)
	}
	span.SetAttributes(attribute.Bool("engine.access.changed", changed), attribute.Int64("engine.authorization.revision", revision))
	return TeamBindingMutationResult{Binding: binding, AuthorizationRevision: revision, Changed: changed}, nil
}

func ensureTeamBindingOwnerSafety(ctx context.Context, tx pgx.Tx, resourceType accesscontrol.ResourceType, changed bool) error {
	if resourceType != accesscontrol.ResourceWorkspace {
		return nil
	}
	return ensureWorkspaceOwnerSafety(ctx, tx, changed)
}

func ensureWorkspaceOwnerSafety(ctx context.Context, tx pgx.Tx, changed bool) error {
	if !changed {
		return nil
	}
	return ensureEffectiveOwnerRemains(ctx, tx)
}

func teamBindingOperation(add bool) string {
	if add {
		return "grant"
	}
	return "revoke"
}

func validateResolvedTeamBinding(add bool, status TeamStatus, resourceExists bool) error {
	if !add {
		return nil
	}
	if status == TeamStatusArchived {
		return ErrTeamArchived
	}
	if !resourceExists {
		return ErrInvalidTeamBinding
	}
	return nil
}

func finalizeTeamBindingMutation(ctx context.Context, tx pgx.Tx, input TeamBindingMutation, operation string, changed bool) (int64, error) {
	revision, err := bumpAuthorizationRevision(ctx, tx, changed)
	if err != nil {
		return 0, err
	}
	if err := auditTeamBindingMutation(ctx, tx, input, operation, revision, changed); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit team binding %s: %w", operation, err)
	}
	return revision, nil
}

func resolveTeamBindingTarget(ctx context.Context, tx pgx.Tx, input TeamBindingMutation) (TeamBinding, uuid.UUID, TeamStatus, bool, error) {
	var binding TeamBinding
	var roleID uuid.UUID
	var status TeamStatus
	var displayName *string
	var resourceExists bool
	err := tx.QueryRow(ctx, `
		SELECT role.id, role.display_name, team.status,
			CASE $3::text
				WHEN 'workspace' THEN workspace.name
				WHEN 'service' THEN service.service_name
				WHEN 'bucket' THEN bucket.name
				WHEN 'artifact' THEN artifact.name
			END,
			CASE $3::text
				WHEN 'workspace' THEN workspace.id IS NOT NULL
				WHEN 'service' THEN service.service_id IS NOT NULL
				WHEN 'bucket' THEN bucket.id IS NOT NULL
				WHEN 'artifact' THEN artifact.artifact_id IS NOT NULL
				ELSE false
			END
		FROM fused_teams team
		JOIN fused_roles role ON role.slug = $2 AND role.system_role = true AND role.scope_type = $3
		LEFT JOIN fused_workspaces workspace ON $3 = 'workspace' AND workspace.id = $4
		LEFT JOIN fused_workspace_services service ON $3 = 'service' AND service.service_id = $4
		LEFT JOIN fused_buckets bucket ON $3 = 'bucket' AND bucket.id = $4
		LEFT JOIN fused_artifact_scopes artifact ON $3 = 'artifact' AND artifact.artifact_id = $4
		WHERE team.id = $1
		FOR UPDATE OF team
	`, input.TeamID, input.RoleSlug, input.Resource.Type, input.Resource.ID).Scan(
		&roleID, &binding.RoleDisplayName, &status, &displayName, &resourceExists,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TeamBinding{}, uuid.Nil, "", false, ErrTeamNotFound
	}
	if err != nil {
		return TeamBinding{}, uuid.Nil, "", false, fmt.Errorf("resolve team binding target: %w", err)
	}
	binding.TeamID = input.TeamID
	binding.RoleSlug = input.RoleSlug
	binding.Resource = input.Resource
	if displayName != nil {
		binding.ResourceDisplayName = *displayName
	}
	return binding, roleID, status, resourceExists, nil
}

func writeTeamBinding(ctx context.Context, tx pgx.Tx, binding *TeamBinding, roleID, actorSubjectID uuid.UUID, add bool) (bool, error) {
	arguments := []any{binding.TeamID, roleID, binding.Resource.Type, binding.Resource.ID, actorSubjectID}
	query := `
		WITH deleted AS (
			DELETE FROM fused_role_bindings
			WHERE subject_type = 'team' AND subject_id = $1
				AND resource_type = $3 AND resource_id = $4 AND role_id <> $2
			RETURNING id
		), inserted AS (
			INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id, created_by_subject_id)
			VALUES ('team', $1, $2, $3, $4, $5)
			ON CONFLICT (subject_type, subject_id, role_id, resource_type, resource_id) DO NOTHING
			RETURNING id, created_at
		)
		SELECT id, created_at, true FROM inserted
		UNION ALL
		SELECT id, created_at, EXISTS (SELECT 1 FROM deleted) FROM fused_role_bindings
		WHERE subject_type = 'team' AND subject_id = $1 AND role_id = $2 AND resource_type = $3 AND resource_id = $4
			AND NOT EXISTS (SELECT 1 FROM inserted)
		LIMIT 1
	`
	if !add {
		query = `DELETE FROM fused_role_bindings
			WHERE subject_type = 'team' AND subject_id = $1 AND role_id = $2 AND resource_type = $3 AND resource_id = $4
			RETURNING id, created_at, true`
		arguments = arguments[:4]
	}
	var changed bool
	err := tx.QueryRow(ctx, query, arguments...).Scan(
		&binding.ID, &binding.CreatedAt, &changed,
	)
	if !add && errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("write team binding: %w", err)
	}
	return changed, nil
}

func (s *postgresStore) beginTeamMutation(ctx context.Context, actor MutationActor) (pgx.Tx, error) {
	if err := validateMutationActor(actor); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin team mutation: %w", err)
	}
	var valid bool
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM fused_control_credentials credential
			JOIN fused_subjects subject ON subject.id = credential.subject_id
			WHERE credential.id = $1 AND credential.subject_id = $2
				AND credential.revoked_at IS NULL
				AND (credential.expires_at IS NULL OR credential.expires_at > NOW())
				AND subject.status = 'active'
		)
	`, actor.CredentialID, actor.SubjectID).Scan(&valid)
	if err != nil || !valid {
		_ = tx.Rollback(ctx)
		if err != nil {
			return nil, fmt.Errorf("validate team mutation actor: %w", err)
		}
		return nil, ErrInvalidMutationActor
	}
	return tx, nil
}

func bumpAuthorizationRevision(ctx context.Context, tx pgx.Tx, changed bool) (int64, error) {
	query := `SELECT revision FROM fused_authorization_state WHERE singleton_key = 1`
	if changed {
		query = `UPDATE fused_authorization_state SET revision = revision + 1, updated_at = NOW() WHERE singleton_key = 1 RETURNING revision`
	}
	var revision int64
	if err := tx.QueryRow(ctx, query).Scan(&revision); err != nil {
		return 0, fmt.Errorf("update authorization revision: %w", err)
	}
	return revision, nil
}

func auditTeamMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, teamID uuid.UUID, revision int64, changed bool) error {
	var workspaceID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM fused_workspaces WHERE singleton_key = 1`).Scan(&workspaceID); err != nil {
		return fmt.Errorf("load workspace for team audit: %w", err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type,
			resource_id, request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, $3, 'access.manage', 'workspace', $4, $5, $6, 'succeeded',
			jsonb_build_object('team_id', $7::text, 'authorization_revision', $8::bigint, 'changed', $9::boolean))
	`, actor.SubjectID, actor.CredentialID, action, workspaceID, actor.RequestID, actor.TraceID, teamID.String(), revision, changed)
	if err != nil {
		return fmt.Errorf("audit team mutation: %w", err)
	}
	return nil
}

func auditTeamBindingMutation(ctx context.Context, tx pgx.Tx, input TeamBindingMutation, operation string, revision int64, changed bool) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type,
			resource_id, request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, $3, 'access.manage', $4, $5, $6, $7, 'succeeded',
			jsonb_build_object('team_id', $8::text, 'role_slug', $9::text,
				'authorization_revision', $10::bigint, 'changed', $11::boolean))
	`, input.Actor.SubjectID, input.Actor.CredentialID, "team.binding."+operation,
		input.Resource.Type, input.Resource.ID, input.Actor.RequestID, input.Actor.TraceID, input.TeamID.String(), input.RoleSlug, revision, changed)
	if err != nil {
		return fmt.Errorf("audit team binding mutation: %w", err)
	}
	return nil
}

func isTeamSlugViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505" && postgresError.ConstraintName == "fused_teams_slug_key"
}

func recordTeamSpanError(span trace.Span, err error) error {
	span.RecordError(err)
	span.SetStatus(codes.Error, "team repository operation failed")
	return err
}

const teamJoinsSQL = `
	LEFT JOIN fused_role_bindings binding ON binding.subject_type = 'team' AND binding.subject_id = team.id
	LEFT JOIN fused_roles role ON role.id = binding.role_id
	LEFT JOIN fused_workspaces workspace ON binding.resource_type = 'workspace' AND workspace.id = binding.resource_id
	LEFT JOIN fused_workspace_services service ON binding.resource_type = 'service' AND service.service_id = binding.resource_id
	LEFT JOIN fused_buckets bucket ON binding.resource_type = 'bucket' AND bucket.id = binding.resource_id
	LEFT JOIN fused_artifact_scopes artifact ON binding.resource_type = 'artifact' AND artifact.artifact_id = binding.resource_id
`

const teamSelectColumnsSQL = `
	SELECT team.id, team.name, team.slug, team.description, team.status, team.created_at, team.updated_at,
		binding.id, binding.resource_type, binding.resource_id, binding.created_at,
		role.slug, role.display_name,
		CASE binding.resource_type
			WHEN 'workspace' THEN workspace.name
			WHEN 'service' THEN service.service_name
			WHEN 'bucket' THEN bucket.name
			WHEN 'artifact' THEN artifact.name
		END
	FROM `

const teamSelectSQL = teamSelectColumnsSQL + `fused_teams team` + teamJoinsSQL

const teamFilterSQL = `
	(array_length($1::text[], 1) IS NULL OR status = ANY($1::text[]))
	AND ($2 = '' OR name ILIKE '%' || $2 || '%' OR slug ILIKE '%' || $2 || '%')
`

const teamCountSQL = `SELECT COUNT(*) FROM fused_teams WHERE ` + teamFilterSQL

const teamListSQL = teamSelectColumnsSQL + `(
	SELECT * FROM fused_teams WHERE ` + teamFilterSQL + `
	ORDER BY created_at DESC, id LIMIT $3 OFFSET $4
) team` + teamJoinsSQL + ` ORDER BY team.created_at DESC, team.id, binding.created_at, binding.id`

func scanTeams(rows pgx.Rows) ([]Team, error) {
	teams := make([]Team, 0)
	indexes := make(map[uuid.UUID]int)
	for rows.Next() {
		team, binding, err := scanTeamRow(rows)
		if err != nil {
			return nil, err
		}
		index, found := indexes[team.ID]
		if !found {
			team.Bindings = []TeamBinding{}
			indexes[team.ID] = len(teams)
			teams = append(teams, team)
			index = len(teams) - 1
		}
		if binding != nil {
			teams[index].Bindings = append(teams[index].Bindings, *binding)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate teams: %w", err)
	}
	return teams, nil
}

func scanTeamRow(row pgx.Row) (Team, *TeamBinding, error) {
	var team Team
	var bindingID, resourceID *uuid.UUID
	var resourceType, roleSlug, roleDisplayName, resourceDisplayName *string
	var bindingCreatedAt *time.Time
	err := row.Scan(
		&team.ID, &team.Name, &team.Slug, &team.Description, &team.Status, &team.CreatedAt, &team.UpdatedAt,
		&bindingID, &resourceType, &resourceID, &bindingCreatedAt, &roleSlug, &roleDisplayName, &resourceDisplayName,
	)
	if err != nil {
		return Team{}, nil, fmt.Errorf("scan team: %w", err)
	}
	if bindingID == nil {
		return team, nil, nil
	}
	binding := &TeamBinding{ID: *bindingID, TeamID: team.ID, RoleSlug: *roleSlug, RoleDisplayName: *roleDisplayName, CreatedAt: *bindingCreatedAt}
	binding.Resource = accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(*resourceType), ID: *resourceID}
	if resourceDisplayName != nil {
		binding.ResourceDisplayName = *resourceDisplayName
	}
	return team, binding, nil
}
