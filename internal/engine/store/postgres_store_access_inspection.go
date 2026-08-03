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

func (s *postgresStore) ExplainAccess(ctx context.Context, input AccessExplanationQuery) (AccessExplanation, error) {
	if err := validateExplanationQuery(input); err != nil {
		return AccessExplanation{}, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.explain")
	defer span.End()
	span.SetAttributes(attribute.String("permission", string(input.Requirement.Permission)), attribute.String("resource_type", string(input.Requirement.Resource.Type)))
	rows, err := s.db.Query(ctx, accessExplanationSQL, input.RequesterSubjectID, input.TargetSubjectID,
		input.Requirement.Permission, input.Requirement.Resource.Type, input.Requirement.Resource.ID, visibilityPermission(input.Requirement.Resource.Type))
	if err != nil {
		return AccessExplanation{}, fmt.Errorf("explain access: %w", err)
	}
	defer rows.Close()
	explanation := AccessExplanation{Requirement: input.Requirement, Sources: []AccessGrantSource{}}
	found := false
	for rows.Next() {
		found = true
		var principalType, teamName, roleSlug, resourceType *string
		var principalID, resourceID *uuid.UUID
		if err := rows.Scan(&principalType, &principalID, &teamName, &roleSlug, &resourceType, &resourceID); err != nil {
			return AccessExplanation{}, fmt.Errorf("scan access explanation: %w", err)
		}
		if principalID != nil {
			explanation.Sources = append(explanation.Sources, AccessGrantSource{PrincipalType: *principalType, PrincipalID: *principalID,
				TeamName: stringValue(teamName), RoleSlug: *roleSlug, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(*resourceType), ID: *resourceID}})
		}
	}
	if err := rows.Err(); err != nil {
		return AccessExplanation{}, fmt.Errorf("iterate access explanation: %w", err)
	}
	if !found {
		return AccessExplanation{}, ErrAccessExplanationHidden
	}
	explanation.Allowed = len(explanation.Sources) > 0
	if !explanation.Allowed {
		explanation.Missing = []accesscontrol.Requirement{input.Requirement}
	}
	return explanation, nil
}

func visibilityPermission(resourceType accesscontrol.ResourceType) accesscontrol.Permission {
	switch resourceType {
	case accesscontrol.ResourceService:
		return accesscontrol.PermissionServiceRead
	case accesscontrol.ResourceBucket:
		return accesscontrol.PermissionBucketRead
	case accesscontrol.ResourceArtifact:
		return accesscontrol.PermissionArtifactRead
	default:
		return accesscontrol.PermissionWorkspaceRead
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

const accessExplanationSQL = `
WITH workspace AS (SELECT id FROM fused_workspaces WHERE singleton_key = 1),
requester_principals(subject_type, subject_id) AS (
	SELECT 'subject'::text, subject.id FROM fused_subjects subject WHERE subject.id = $1 AND subject.status = 'active'
	UNION ALL
	SELECT 'team'::text, membership.team_id FROM fused_team_memberships membership
	JOIN fused_subjects subject ON subject.id = membership.member_subject_id AND subject.status = 'active'
	JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
	WHERE membership.member_subject_id = $1
	UNION ALL
	SELECT 'workspace'::text, workspace.id FROM workspace
	JOIN fused_subjects subject ON subject.id = $1 AND subject.status = 'active'
), requester_grants AS (
	SELECT permission.permission, binding.resource_type, binding.resource_id
	FROM requester_principals principal
	JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = role.id
), guarded AS (
	SELECT EXISTS (SELECT 1 FROM requester_grants effective JOIN workspace ON true
		WHERE effective.permission = 'access.read' AND effective.resource_type = 'workspace' AND effective.resource_id = workspace.id)
	AND EXISTS (SELECT 1 FROM requester_grants effective WHERE effective.permission = $6
		AND (effective.resource_type = 'workspace' OR (effective.resource_type = $4 AND effective.resource_id = $5))) AS allowed
), target_principals(subject_type, subject_id) AS (
	SELECT 'subject'::text, subject.id FROM fused_subjects subject WHERE subject.id = $2 AND subject.status = 'active'
	UNION ALL
	SELECT 'team'::text, membership.team_id FROM fused_team_memberships membership
	JOIN fused_subjects subject ON subject.id = membership.member_subject_id AND subject.status = 'active'
	JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
	WHERE membership.member_subject_id = $2
	UNION ALL
	SELECT 'workspace'::text, workspace.id FROM workspace
	JOIN fused_subjects subject ON subject.id = $2 AND subject.status = 'active'
), matching AS (
	SELECT principal.subject_type, principal.subject_id, team.name, role.slug, binding.resource_type, binding.resource_id
	FROM target_principals principal
	JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
	JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = $3
	LEFT JOIN fused_teams team ON principal.subject_type = 'team' AND team.id = principal.subject_id
	WHERE binding.resource_type = 'workspace' OR (binding.resource_type = $4 AND binding.resource_id = $5)
)
SELECT matching.subject_type, matching.subject_id, matching.name, matching.slug, matching.resource_type, matching.resource_id
FROM guarded LEFT JOIN matching ON true WHERE guarded.allowed
ORDER BY matching.subject_type, matching.subject_id, matching.slug, matching.resource_type, matching.resource_id`

var _ AccessInspectionRepository = (*postgresStore)(nil)

func hiddenExplanationError(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
