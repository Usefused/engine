package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
)

func (s *postgresStore) AddTeamMember(ctx context.Context, input TeamMemberMutation) (MembershipMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_member.add")
	defer span.End()
	if err := validateTeamMemberMutation(input); err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginTeamMutation(ctx, input.Actor)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	team, err := loadOwnerAuthorizedMembershipTeam(ctx, tx, input.TeamID, input.Actor.SubjectID, true)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err := loadUserForMutation(ctx, tx, input.UserID)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	if user.Status == UserStatusArchived {
		return MembershipMutationResult{}, ErrUserArchived
	}
	membership, changed, err := upsertTeamMembership(ctx, tx, team, user.ID, input.Role, input.Actor.SubjectID)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	return finalizeMembershipMutation(ctx, tx, membershipMutationState{input: input, user: user, membership: membership, changed: changed, action: "team.member.add"})
}

func validateTeamMemberMutation(input TeamMemberMutation) error {
	if input.TeamID == uuid.Nil || input.UserID == uuid.Nil {
		return ErrInvalidTeamMembership
	}
	if err := validateMembershipRole(input.Role); err != nil {
		return err
	}
	return validateMutationActor(input.Actor)
}

func (s *postgresStore) AddTeamMemberByEmail(ctx context.Context, input AddTeamMemberByEmailInput) (MembershipMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_member.add_by_email")
	defer span.End()
	normalized, display, displayName, err := validateAddTeamMemberByEmail(input)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	tx, err := s.beginTeamMutation(ctx, input.Actor)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	team, err := loadOwnerAuthorizedMembershipTeam(ctx, tx, input.TeamID, input.Actor.SubjectID, true)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	user, createdUser, err := loadOrCreateInvitedUser(ctx, tx, normalized, display, displayName)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	if user.Status == UserStatusArchived {
		return MembershipMutationResult{}, ErrUserArchived
	}
	membership, membershipChanged, err := upsertTeamMembership(ctx, tx, team, user.ID, input.Role, input.Actor.SubjectID)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	state := membershipMutationState{
		input: TeamMemberMutation{TeamID: input.TeamID, UserID: user.ID, Role: input.Role, Actor: input.Actor},
		user:  user, membership: membership, changed: membershipChanged || createdUser, createdUser: createdUser, action: "team.member.add",
	}
	return finalizeMembershipMutation(ctx, tx, state)
}

func validateAddTeamMemberByEmail(input AddTeamMemberByEmailInput) (string, string, string, error) {
	if input.TeamID == uuid.Nil {
		return "", "", "", ErrInvalidTeamMembership
	}
	normalized, display, err := normalizeUserEmail(input.Email)
	if err != nil {
		return "", "", "", err
	}
	displayName := input.DisplayName
	if displayName == "" {
		displayName = generatedUserDisplayName(display)
	}
	if err := validateUserDisplayName(displayName); err != nil {
		return "", "", "", err
	}
	if err := validateMembershipRole(input.Role); err != nil {
		return "", "", "", err
	}
	if err := validateMutationActor(input.Actor); err != nil {
		return "", "", "", err
	}
	return normalized, display, displayName, nil
}

func loadOrCreateInvitedUser(ctx context.Context, tx pgx.Tx, normalized, email, displayName string) (User, bool, error) {
	var userID uuid.UUID
	// Lock the normalized email before deciding to insert so concurrent add-by-
	// email requests converge on one subject instead of creating duplicates or
	// performing an application-side check-then-create race.
	err := tx.QueryRow(ctx, `SELECT subject_id FROM fused_users WHERE email_normalized = $1 FOR UPDATE`, normalized).Scan(&userID)
	if err == nil {
		user, err := loadUserForMutation(ctx, tx, userID)
		return user, false, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, false, fmt.Errorf("find user by email: %w", err)
	}
	user, err := insertInvitedUser(ctx, tx, normalized, email, displayName)
	return user, err == nil, err
}

func (s *postgresStore) RemoveTeamMember(ctx context.Context, teamID, userID uuid.UUID, actor MutationActor) (MembershipMutationResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_member.remove")
	defer span.End()
	if teamID == uuid.Nil || userID == uuid.Nil || validateMutationActor(actor) != nil {
		return MembershipMutationResult{}, ErrInvalidTeamMembership
	}
	tx, err := s.beginTeamMutation(ctx, actor)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	team, err := loadOwnerAuthorizedMembershipTeam(ctx, tx, teamID, actor.SubjectID, false)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	user, err := loadUserForMutation(ctx, tx, userID)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	membership, changed, err := deleteTeamMembership(ctx, tx, team, userID)
	if err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	if err := ensureWorkspaceOwnerSafety(ctx, tx, changed); err != nil {
		return MembershipMutationResult{}, recordTeamSpanError(span, err)
	}
	state := membershipMutationState{
		input: TeamMemberMutation{TeamID: teamID, UserID: userID, Role: membership.Role, Actor: actor},
		user:  user, membership: membership, changed: changed, action: "team.member.remove",
	}
	return finalizeMembershipMutation(ctx, tx, state)
}

type membershipTeam struct {
	ID     uuid.UUID
	Name   string
	Slug   string
	Status TeamStatus
}

func loadMembershipTeam(ctx context.Context, tx pgx.Tx, teamID uuid.UUID, requireActive bool) (membershipTeam, error) {
	var team membershipTeam
	err := tx.QueryRow(ctx, `SELECT id, name, slug, status FROM fused_teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&team.ID, &team.Name, &team.Slug, &team.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return membershipTeam{}, ErrTeamNotFound
	}
	if err != nil {
		return membershipTeam{}, fmt.Errorf("load membership team: %w", err)
	}
	if requireActive && team.Status == TeamStatusArchived {
		return membershipTeam{}, ErrTeamArchived
	}
	return team, nil
}

func upsertTeamMembership(ctx context.Context, tx pgx.Tx, team membershipTeam, userID uuid.UUID, role MembershipRole, actorID uuid.UUID) (TeamMembership, bool, error) {
	var membership TeamMembership
	var changed bool
	err := tx.QueryRow(ctx, `
		WITH changed AS (
			INSERT INTO fused_team_memberships (team_id, member_subject_id, membership_role, created_by_subject_id)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (team_id, member_subject_id) DO UPDATE SET membership_role = EXCLUDED.membership_role
			WHERE fused_team_memberships.membership_role IS DISTINCT FROM EXCLUDED.membership_role
			RETURNING membership_role, created_at
		)
		SELECT membership_role, created_at, true FROM changed
		UNION ALL
		SELECT membership_role, created_at, false FROM fused_team_memberships
		WHERE team_id = $1 AND member_subject_id = $2 AND NOT EXISTS (SELECT 1 FROM changed)
		LIMIT 1
	`, team.ID, userID, role, actorID).Scan(&membership.Role, &membership.CreatedAt, &changed)
	if err != nil {
		return TeamMembership{}, false, fmt.Errorf("upsert team membership: %w", err)
	}
	membership.TeamID, membership.TeamName, membership.TeamSlug, membership.TeamStatus = team.ID, team.Name, team.Slug, team.Status
	return membership, changed, nil
}

func deleteTeamMembership(ctx context.Context, tx pgx.Tx, team membershipTeam, userID uuid.UUID) (TeamMembership, bool, error) {
	membership := TeamMembership{TeamID: team.ID, TeamName: team.Name, TeamSlug: team.Slug, TeamStatus: team.Status}
	err := tx.QueryRow(ctx, `
		DELETE FROM fused_team_memberships WHERE team_id = $1 AND member_subject_id = $2
		RETURNING membership_role, created_at
	`, team.ID, userID).Scan(&membership.Role, &membership.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return membership, false, nil
	}
	if err != nil {
		return TeamMembership{}, false, fmt.Errorf("delete team membership: %w", err)
	}
	return membership, true, nil
}

type membershipMutationState struct {
	input       TeamMemberMutation
	user        User
	membership  TeamMembership
	changed     bool
	createdUser bool
	action      string
}

func finalizeMembershipMutation(ctx context.Context, tx pgx.Tx, state membershipMutationState) (MembershipMutationResult, error) {
	user, err := loadHydratedUser(ctx, tx, state.user.ID)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	state.user = user
	revision, err := bumpAuthorizationRevision(ctx, tx, state.changed)
	if err != nil {
		return MembershipMutationResult{}, err
	}
	if state.createdUser {
		if err := auditUserMutation(ctx, tx, state.input.Actor, "user.create", state.user.ID, revision, true); err != nil {
			return MembershipMutationResult{}, err
		}
	}
	if err := auditMembershipMutation(ctx, tx, state.input.Actor, state.action, state.user.ID, state.membership, revision, state.changed, state.createdUser); err != nil {
		return MembershipMutationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MembershipMutationResult{}, fmt.Errorf("commit membership mutation: %w", err)
	}
	return MembershipMutationResult{User: state.user, Membership: state.membership, AuthorizationRevision: revision, Changed: state.changed, CreatedUser: state.createdUser}, nil
}

func auditMembershipMutation(ctx context.Context, tx pgx.Tx, actor MutationActor, action string, userID uuid.UUID, membership TeamMembership, revision int64, changed, createdUser bool) error {
	workspaceID, err := workspaceIDForAudit(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type, resource_id,
			request_id, trace_id, outcome, metadata
		) VALUES ($1, $2, $3, 'access.manage', 'workspace', $4, $5, $6, 'succeeded',
			jsonb_build_object('user_id', $7::text, 'team_id', $8::text, 'membership_role', $9::text,
				'authorization_revision', $10::bigint, 'changed', $11::boolean, 'created_user', $12::boolean))
	`, actor.SubjectID, actor.CredentialID, action, workspaceID, actor.RequestID, actor.TraceID,
		userID.String(), membership.TeamID.String(), string(membership.Role), revision, changed, createdUser)
	if err != nil {
		return fmt.Errorf("audit membership mutation: %w", err)
	}
	return nil
}
