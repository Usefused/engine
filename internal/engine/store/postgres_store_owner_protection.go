package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var ErrOwnerManagementForbidden = errors.New("workspace Owner management requires account.manage")

const ownerProtectionSQL = `
	WITH target_principals AS (
		SELECT $1::text AS subject_type, $2::uuid AS subject_id
		UNION ALL
		SELECT 'team'::text, membership.team_id
		FROM fused_team_memberships membership
		WHERE $1::text = 'subject' AND membership.member_subject_id = $2
	), actor_principals AS (
		SELECT 'subject'::text AS subject_type, subject.id AS subject_id
		FROM fused_subjects subject WHERE subject.id = $3 AND subject.status = 'active'
		UNION ALL
		SELECT 'team'::text, membership.team_id
		FROM fused_team_memberships membership
		JOIN fused_subjects subject ON subject.id = membership.member_subject_id AND subject.status = 'active'
		JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
		WHERE membership.member_subject_id = $3
	), target_protected AS (
		SELECT EXISTS (
			SELECT 1 FROM target_principals principal
			JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
			JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
			JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'account.manage'
			JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
			WHERE binding.resource_type = 'workspace'
		) AS protected
	), actor_allowed AS (
		SELECT EXISTS (
			SELECT 1 FROM actor_principals principal
			JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
			JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
			JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'account.manage'
			JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
			WHERE binding.resource_type = 'workspace'
		) AS allowed
	)
	SELECT target_protected.protected, actor_allowed.allowed FROM target_protected CROSS JOIN actor_allowed
`

func ensureOwnerProtectedPrincipalAuthorized(ctx context.Context, tx pgx.Tx, targetType string, targetID, actorID uuid.UUID, ownerRequested bool) error {
	if targetType != "subject" && targetType != "team" {
		return fmt.Errorf("invalid owner-protection target %q", targetType)
	}
	var targetProtected, actorAllowed bool
	if err := tx.QueryRow(ctx, ownerProtectionSQL, targetType, targetID, actorID).Scan(&targetProtected, &actorAllowed); err != nil {
		return fmt.Errorf("check Owner management authorization: %w", err)
	}
	// Existing Owner principals and newly requested Owner roles are protected;
	// ordinary access-management mutations keep the faster Admin path.
	if (targetProtected || ownerRequested) && !actorAllowed {
		return ErrOwnerManagementForbidden
	}
	return nil
}

func ensureOwnerTeamAuthorized(ctx context.Context, tx pgx.Tx, teamID, actorID uuid.UUID, ownerRequested bool) error {
	return ensureOwnerProtectedPrincipalAuthorized(ctx, tx, "team", teamID, actorID, ownerRequested)
}

func ensureOwnerUserAuthorized(ctx context.Context, tx pgx.Tx, userID, actorID uuid.UUID) error {
	return ensureOwnerProtectedPrincipalAuthorized(ctx, tx, "subject", userID, actorID, false)
}

func loadOwnerAuthorizedUser(ctx context.Context, tx pgx.Tx, userID, actorID uuid.UUID) (User, error) {
	user, err := loadUserForMutation(ctx, tx, userID)
	if err != nil {
		return User{}, err
	}
	if err := ensureOwnerUserAuthorized(ctx, tx, user.ID, actorID); err != nil {
		return User{}, err
	}
	return user, nil
}

func loadOwnerAuthorizedMembershipTeam(ctx context.Context, tx pgx.Tx, teamID, actorID uuid.UUID, requireActive bool) (membershipTeam, error) {
	team, err := loadMembershipTeam(ctx, tx, teamID, requireActive)
	if err != nil {
		return membershipTeam{}, err
	}
	if err := ensureOwnerTeamAuthorized(ctx, tx, team.ID, actorID, false); err != nil {
		return membershipTeam{}, err
	}
	return team, nil
}

func loadOwnerAuthorizedWorkspaceRole(ctx context.Context, tx pgx.Tx, teamID, workspaceID, actorID uuid.UUID) (TeamBinding, error) {
	binding, status, workspaceExists, err := loadTeamWorkspaceRole(ctx, tx, teamID, workspaceID)
	if err != nil {
		return TeamBinding{}, err
	}
	if err := validateClearTeamWorkspaceRole(status, workspaceExists); err != nil {
		return TeamBinding{}, err
	}
	if err := ensureOwnerTeamAuthorized(ctx, tx, teamID, actorID, false); err != nil {
		return TeamBinding{}, err
	}
	return binding, nil
}

func lockOwnerAuthorizedTeamForArchive(ctx context.Context, tx pgx.Tx, teamID, actorID uuid.UUID) (Team, int, int, error) {
	team, bindingCount, activeAppCount, err := lockTeamForArchive(ctx, tx, teamID)
	if err != nil {
		return Team{}, 0, 0, err
	}
	if err := ensureOwnerTeamAuthorized(ctx, tx, teamID, actorID, false); err != nil {
		return Team{}, 0, 0, err
	}
	return team, bindingCount, activeAppCount, nil
}

func ownerRoleRequested(resource accesscontrol.ResourceRef, role string) bool {
	return resource.Type == accesscontrol.ResourceWorkspace && role == accesscontrol.RoleOwner
}
