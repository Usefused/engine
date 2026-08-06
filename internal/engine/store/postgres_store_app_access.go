package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) PreflightAppOwnership(ctx context.Context, input AppOwnershipPreflight) (AppOwnershipDecision, error) {
	if err := validateOwnershipPreflight(input); err != nil {
		return AppOwnershipDecision{}, err
	}
	permissions, resourceTypes, resourceIDs := requirementColumns(input.Requirements)
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.app_ownership.preflight")
	defer span.End()
	span.SetAttributes(attribute.Int("requirement_count", len(input.Requirements)), attribute.String("team_id", input.OwnerTeamID.String()))

	// The database evaluates actor grants, owning-team grants, active status, and
	// membership together. Splitting these reads would introduce authorization
	// races and make the result dependent on application-side filtering.
	rows, err := s.db.Query(ctx, appOwnershipPreflightSQL, input.ActorSubjectID, input.OwnerTeamID, permissions, resourceTypes, resourceIDs, input.ExistingAppID)
	if err != nil {
		return AppOwnershipDecision{}, fmt.Errorf("preflight app ownership: %w", err)
	}
	defer rows.Close()
	decision, err := scanOwnershipDecision(rows)
	if err != nil {
		return AppOwnershipDecision{}, err
	}
	span.SetAttributes(attribute.Bool("allowed", decision.Allowed), attribute.Int("actor_missing", len(decision.ActorMissing)), attribute.Int("team_missing", len(decision.TeamMissing)))
	return decision, nil
}

func (s *postgresStore) ResolveAppFamilyAccess(ctx context.Context, accountID uuid.UUID, appIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	if accountID == uuid.Nil || len(appIDs) == 0 {
		return nil, ErrInvalidAppAccessRequest
	}
	rows, err := s.db.Query(ctx, `
		SELECT app_id, app_family_id
		FROM (
			SELECT app_id, app_family_id, account_id FROM fused_apps
			UNION ALL
			SELECT app_id, app_family_id, account_id FROM fused_app_tombstones
		) app_identity
		WHERE account_id = $1 AND app_id = ANY($2::uuid[])
	`, accountID, appIDs)
	if err != nil {
		return nil, fmt.Errorf("resolve app family access: %w", err)
	}
	defer rows.Close()
	resolved := make(map[uuid.UUID]uuid.UUID, len(appIDs))
	for rows.Next() {
		var appID, familyID uuid.UUID
		if err := rows.Scan(&appID, &familyID); err != nil {
			return nil, fmt.Errorf("scan app family access: %w", err)
		}
		resolved[appID] = familyID
	}
	return resolved, rows.Err()
}

func requirementColumns(requirements []accesscontrol.Requirement) ([]string, []string, []uuid.UUID) {
	permissions := make([]string, len(requirements))
	resourceTypes := make([]string, len(requirements))
	resourceIDs := make([]uuid.UUID, len(requirements))
	for index, requirement := range requirements {
		permissions[index] = string(requirement.Permission)
		resourceTypes[index] = string(requirement.Resource.Type)
		resourceIDs[index] = requirement.Resource.ID
	}
	return permissions, resourceTypes, resourceIDs
}

func scanOwnershipDecision(rows pgx.Rows) (AppOwnershipDecision, error) {
	decision := AppOwnershipDecision{MembershipAllowed: true}
	found := false
	for rows.Next() {
		found = true
		row, err := scanOwnershipDecisionRow(rows)
		if err != nil {
			return AppOwnershipDecision{}, fmt.Errorf("scan app ownership preflight: %w", err)
		}
		if !row.membershipAllowed() {
			decision.MembershipAllowed = false
		}
		if !row.actorAllowed {
			decision.ActorMissing = append(decision.ActorMissing, row.requirement)
		}
		if !row.teamAllowed {
			decision.TeamMissing = append(decision.TeamMissing, row.requirement)
		}
	}
	if err := rows.Err(); err != nil {
		return AppOwnershipDecision{}, fmt.Errorf("iterate app ownership preflight: %w", err)
	}
	decision.Allowed = ownershipAllowed(found, decision)
	return decision, nil
}

type ownershipDecisionRow struct {
	requirement      accesscontrol.Requirement
	actorActive      bool
	teamActive       bool
	memberOrOverride bool
	actorAllowed     bool
	teamAllowed      bool
}

func scanOwnershipDecisionRow(row pgx.Row) (ownershipDecisionRow, error) {
	var scanned ownershipDecisionRow
	return scanned, row.Scan(&scanned.requirement.Permission, &scanned.requirement.Resource.Type, &scanned.requirement.Resource.ID,
		&scanned.actorActive, &scanned.teamActive, &scanned.memberOrOverride, &scanned.actorAllowed, &scanned.teamAllowed)
}

func (row ownershipDecisionRow) membershipAllowed() bool {
	return row.actorActive && row.teamActive && row.memberOrOverride
}

func ownershipAllowed(found bool, decision AppOwnershipDecision) bool {
	// App creation is an intersection: both the caller and the owning team
	// need every resource grant, and the caller must be allowed to act for that
	// team. A union would let either side broaden the other's authority.
	return found && decision.MembershipAllowed && len(decision.ActorMissing) == 0 && len(decision.TeamMissing) == 0
}

func (s *postgresStore) ListAppBuildSelectors(ctx context.Context, input AppSelectorQuery) (AppSelectorPage, error) {
	if err := validateSelectorQuery(input); err != nil {
		return AppSelectorPage{}, err
	}
	permission := accesscontrol.PermissionServiceConsume
	query := appServiceSelectorSQL
	if input.ResourceType == accesscontrol.ResourceBucket {
		permission = accesscontrol.PermissionBucketUse
		query = appBucketSelectorSQL
	}
	rows, err := s.db.Query(ctx, query, input.ActorSubjectID, input.OwnerTeamID, permission, input.Search, input.Limit, input.Offset)
	if err != nil {
		return AppSelectorPage{}, fmt.Errorf("list app build selectors: %w", err)
	}
	defer rows.Close()
	page := AppSelectorPage{Items: make([]AppBuildSelector, 0, input.Limit)}
	for rows.Next() {
		var resourceID *uuid.UUID
		var displayName *string
		if err := rows.Scan(&resourceID, &displayName, &page.Total); err != nil {
			return AppSelectorPage{}, fmt.Errorf("scan app build selector: %w", err)
		}
		if resourceID != nil {
			page.Items = append(page.Items, AppBuildSelector{Resource: accesscontrol.ResourceRef{Type: input.ResourceType, ID: *resourceID}, DisplayName: *displayName})
		}
	}
	return page, rows.Err()
}

func (s *postgresStore) ListAppOwningTeams(ctx context.Context, input ActorTeamSelectorQuery) (AppOwningTeamPage, error) {
	if err := validateOwningTeamQuery(input); err != nil {
		return AppOwningTeamPage{}, err
	}
	rows, err := s.db.Query(ctx, appOwningTeamSelectorSQL, input.ActorSubjectID, input.Search, input.Limit, input.Offset)
	if err != nil {
		return AppOwningTeamPage{}, fmt.Errorf("list app owning teams: %w", err)
	}
	defer rows.Close()
	page := AppOwningTeamPage{Items: make([]AppOwningTeam, 0, input.Limit)}
	for rows.Next() {
		var id *uuid.UUID
		var name, slug *string
		if err := rows.Scan(&id, &name, &slug, &page.Total); err != nil {
			return AppOwningTeamPage{}, fmt.Errorf("scan app owning team: %w", err)
		}
		if id != nil {
			page.Items = append(page.Items, AppOwningTeam{ID: *id, Name: *name, Slug: *slug})
		}
	}
	return page, rows.Err()
}

func (s *postgresStore) ResolveAppOwningTeamReference(ctx context.Context, input AppOwningTeamReferenceQuery) (uuid.UUID, error) {
	if err := validateOwningTeamReferenceQuery(input); err != nil {
		return uuid.Nil, err
	}
	exactID, _ := uuid.Parse(input.Reference)
	var teamID uuid.UUID
	err := s.db.QueryRow(ctx, appOwningTeamReferenceSQL, input.ActorSubjectID, exactID, input.Reference).Scan(&teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Existing-but-ineligible and unknown references intentionally share one
		// result so team slugs cannot become an authorization side channel.
		return uuid.Nil, ErrResourceReferenceNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("resolve app owning team reference: %w", err)
	}
	return teamID, nil
}

const appOwnershipPreflightSQL = `
WITH requested AS (
	SELECT DISTINCT permission, resource_type, resource_id
	FROM unnest($3::text[], $4::text[], $5::uuid[]) AS requirement(permission, resource_type, resource_id)
), actor AS (
	SELECT subject.id, subject.status = 'active' AS active
	FROM fused_subjects subject WHERE subject.id = $1
), owning_team AS (
	SELECT team.id, team.status = 'active' AS active
	FROM fused_teams team WHERE team.id = $2
), workspace AS (
	SELECT id FROM fused_workspaces WHERE singleton_key = 1
), actor_principals(subject_type, subject_id) AS (
	SELECT 'subject'::text, actor.id FROM actor
	UNION ALL
	SELECT 'team'::text, membership.team_id FROM actor
	JOIN fused_team_memberships membership ON membership.member_subject_id = actor.id
	JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
	UNION ALL
	SELECT 'workspace'::text, workspace.id FROM workspace JOIN actor ON actor.active
), actor_grants AS (
	SELECT DISTINCT permission.permission, binding.resource_type, binding.resource_id
	FROM actor_principals principal
	JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
), team_grants AS (
	SELECT DISTINCT permission.permission, binding.resource_type, binding.resource_id
	FROM owning_team team
	JOIN fused_role_bindings binding ON binding.subject_type = 'team' AND binding.subject_id = team.id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
	UNION
	-- Workspace shares make a resource eligible for every owning team without
	-- transferring the owner's management authority to those teams.
	SELECT DISTINCT permission.permission, binding.resource_type, binding.resource_id
	FROM workspace
	JOIN fused_role_bindings binding ON binding.subject_type = 'workspace' AND binding.subject_id = workspace.id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
), actor_access_manage AS (
	SELECT EXISTS (
		SELECT 1 FROM actor_grants effective JOIN workspace ON true
		WHERE effective.permission = 'access.manage' AND effective.resource_type = 'workspace' AND effective.resource_id = workspace.id
	) AS allowed
), shared_app_manager AS (
	SELECT EXISTS (
		SELECT 1 FROM actor
		JOIN fused_apps app ON app.app_id = $6
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id AND family.owner_team_id = $2
		JOIN fused_team_memberships membership ON membership.member_subject_id = actor.id
		JOIN fused_teams shared_team ON shared_team.id = membership.team_id AND shared_team.status = 'active'
		JOIN fused_role_bindings binding ON binding.subject_type = 'team' AND binding.subject_id = shared_team.id
			AND binding.resource_type = 'app' AND binding.resource_id = family.app_family_id
		JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'app'
		JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'app.manage'
		WHERE actor.active AND $6::uuid IS NOT NULL
	) AS allowed
)
SELECT requested.permission, requested.resource_type, requested.resource_id,
	COALESCE((SELECT active FROM actor), false), COALESCE((SELECT active FROM owning_team), false),
	(EXISTS (SELECT 1 FROM fused_team_memberships membership WHERE membership.team_id = $2 AND membership.member_subject_id = $1)
		OR (SELECT allowed FROM actor_access_manage) OR (SELECT allowed FROM shared_app_manager)),
	EXISTS (SELECT 1 FROM actor_grants effective WHERE effective.permission = requested.permission
		AND (effective.resource_type = 'workspace' OR (effective.resource_type = requested.resource_type AND effective.resource_id = requested.resource_id))),
	EXISTS (SELECT 1 FROM team_grants effective WHERE effective.permission = requested.permission
		AND (effective.resource_type = 'workspace' OR (effective.resource_type = requested.resource_type AND effective.resource_id = requested.resource_id)))
FROM requested ORDER BY requested.permission, requested.resource_type, requested.resource_id`

const appSelectorAuthorizationSQL = `
WITH actor_principals(subject_type, subject_id) AS (
	SELECT 'subject'::text, subject.id FROM fused_subjects subject WHERE subject.id = $1 AND subject.status = 'active'
	UNION ALL
	SELECT 'team'::text, membership.team_id FROM fused_team_memberships membership
	JOIN fused_subjects subject ON subject.id = membership.member_subject_id AND subject.status = 'active'
	JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
	WHERE membership.member_subject_id = $1
	UNION ALL
	SELECT 'workspace'::text, workspace.id FROM fused_workspaces workspace
	JOIN fused_subjects subject ON subject.id = $1 AND subject.status = 'active'
	WHERE workspace.singleton_key = 1
), actor_grants AS (
	SELECT permission.permission, binding.resource_type, binding.resource_id
	FROM actor_principals principal
	JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
	WHERE permission.permission = $3
), team_grants AS (
	SELECT permission.permission, binding.resource_type, binding.resource_id
	FROM fused_teams team
	JOIN fused_role_bindings binding ON binding.subject_type = 'team' AND binding.subject_id = team.id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
	WHERE team.id = $2 AND team.status = 'active' AND permission.permission = $3
	UNION
	SELECT permission.permission, binding.resource_type, binding.resource_id
	FROM fused_workspaces workspace
	JOIN fused_role_bindings binding ON binding.subject_type = 'workspace' AND binding.subject_id = workspace.id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
	WHERE workspace.singleton_key = 1 AND permission.permission = $3
), owner_eligible AS (
	SELECT EXISTS (
		SELECT 1 FROM fused_subjects subject
		WHERE subject.id = $1 AND subject.status = 'active' AND (
			$2 = '00000000-0000-0000-0000-000000000000'::uuid
			OR (EXISTS (SELECT 1 FROM fused_teams team WHERE team.id = $2 AND team.status = 'active') AND (
				EXISTS (SELECT 1 FROM fused_team_memberships membership WHERE membership.team_id = $2 AND membership.member_subject_id = $1)
				OR EXISTS (SELECT 1 FROM actor_principals principal
				JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
				JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
				JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'access.manage'
				JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
				WHERE binding.resource_type = 'workspace')))
		)
	) AS allowed
) `

const appServiceSelectorSQL = appSelectorAuthorizationSQL + `
,
filtered AS (
	SELECT service.service_id, service.service_name FROM fused_workspace_services service
	WHERE (SELECT allowed FROM owner_eligible)
		AND EXISTS (SELECT 1 FROM fused_workspace_service_versions version WHERE version.service_id = service.service_id)
		AND ($4 = '' OR service.service_name ILIKE '%' || $4 || '%')
		AND EXISTS (SELECT 1 FROM actor_grants effective WHERE effective.resource_type = 'workspace'
			OR (effective.resource_type = 'service' AND effective.resource_id = service.service_id))
		AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR EXISTS (
			SELECT 1 FROM team_grants effective WHERE effective.resource_type = 'workspace'
				OR (effective.resource_type = 'service' AND effective.resource_id = service.service_id)))
), page AS (
	SELECT * FROM filtered ORDER BY service_name, service_id LIMIT $5 OFFSET $6
), summary AS (SELECT COUNT(*)::int AS total FROM filtered)
SELECT page.service_id, page.service_name, summary.total FROM summary LEFT JOIN page ON true
ORDER BY page.service_name, page.service_id`

const appBucketSelectorSQL = appSelectorAuthorizationSQL + `
,
filtered AS (
	SELECT bucket.id, bucket.name FROM fused_buckets bucket
	WHERE ($4 = '' OR bucket.name ILIKE '%' || $4 || '%')
		AND (SELECT allowed FROM owner_eligible)
		AND EXISTS (SELECT 1 FROM actor_grants effective WHERE effective.resource_type = 'workspace'
			OR (effective.resource_type = 'bucket' AND effective.resource_id = bucket.id))
		AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR EXISTS (
			SELECT 1 FROM team_grants effective WHERE effective.resource_type = 'workspace'
				OR (effective.resource_type = 'bucket' AND effective.resource_id = bucket.id)))
), page AS (
	SELECT * FROM filtered ORDER BY name, id LIMIT $5 OFFSET $6
), summary AS (SELECT COUNT(*)::int AS total FROM filtered)
SELECT page.id, page.name, summary.total FROM summary LEFT JOIN page ON true
ORDER BY page.name, page.id`

const appOwningTeamAuthorizationSQL = `
WITH actor AS (
	SELECT id FROM fused_subjects WHERE id = $1 AND status = 'active'
), actor_access_manage AS (
	SELECT EXISTS (
		SELECT 1 FROM fused_role_bindings binding
		JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
		JOIN fused_role_permissions permission ON permission.role_id = binding.role_id
		JOIN fused_workspaces workspace ON workspace.singleton_key = 1
		WHERE permission.permission = 'access.manage' AND binding.resource_type = 'workspace'
			AND binding.resource_id = workspace.id
			AND ((binding.subject_type = 'subject' AND binding.subject_id IN (SELECT id FROM actor)) OR
				(binding.subject_type = 'team' AND binding.subject_id IN (
					SELECT membership.team_id FROM actor
					JOIN fused_team_memberships membership ON membership.member_subject_id = actor.id
					JOIN fused_teams member_team ON member_team.id = membership.team_id AND member_team.status = 'active'
				)))
	) AS allowed
), eligible AS (
	SELECT team.id, team.name, team.slug FROM fused_teams team
	WHERE team.status = 'active' AND ((SELECT allowed FROM actor_access_manage) OR EXISTS (
			SELECT 1 FROM actor JOIN fused_team_memberships membership ON membership.member_subject_id = actor.id
			WHERE membership.team_id = team.id
		))
		AND EXISTS (
			SELECT 1 FROM fused_role_bindings binding
			JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
			JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'app.create'
			JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
			WHERE binding.subject_type = 'team' AND binding.subject_id = team.id AND binding.resource_type = 'workspace'
		)
)`

const appOwningTeamSelectorSQL = appOwningTeamAuthorizationSQL + `
, filtered AS (
	SELECT * FROM eligible
	WHERE $2 = '' OR name ILIKE '%' || $2 || '%' OR slug ILIKE '%' || $2 || '%'
), page AS (
	SELECT * FROM filtered ORDER BY name, id LIMIT $3 OFFSET $4
), summary AS (SELECT COUNT(*)::int AS total FROM filtered)
SELECT page.id, page.name, page.slug, summary.total FROM summary LEFT JOIN page ON true
ORDER BY page.name, page.id`

const appOwningTeamReferenceSQL = appOwningTeamAuthorizationSQL + `
SELECT id FROM eligible WHERE id = $2 OR lower(slug) = lower($3)
ORDER BY (id = $2) DESC LIMIT 1`

var _ AppAccessRepository = (*postgresStore)(nil)
