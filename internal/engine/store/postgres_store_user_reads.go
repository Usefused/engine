package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) GetUser(ctx context.Context, userID uuid.UUID) (User, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.get")
	defer span.End()
	if userID == uuid.Nil {
		return User{}, ErrUserNotFound
	}
	user, err := loadHydratedUser(ctx, s.db, userID)
	if err != nil {
		return User{}, recordTeamSpanError(span, err)
	}
	return user, nil
}

type userRowsQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

func loadHydratedUser(ctx context.Context, querier userRowsQuerier, userID uuid.UUID) (User, error) {
	rows, err := querier.Query(ctx, userSelectSQL+` WHERE user_row.subject_id = $1`, userID)
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	defer rows.Close()
	users, err := scanUsers(rows)
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrUserNotFound
	}
	return users[0], nil
}

func (s *postgresStore) ListUsers(ctx context.Context, options UserListOptions) ([]User, int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.user.list")
	defer span.End()
	statuses, err := validateUserListOptions(&options)
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	batch := &pgx.Batch{}
	batch.Queue(userCountSQL, statuses, options.Search)
	batch.Queue(userListQuery(options.IncludeChildren), statuses, options.Search, options.Limit, options.Offset)
	results := s.db.SendBatch(ctx, batch)
	defer results.Close()
	var total int
	if err := results.QueryRow().Scan(&total); err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("count users: %w", err))
	}
	rows, err := results.Query()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("list users: %w", err))
	}
	users, err := scanUsers(rows)
	rows.Close()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	span.SetAttributes(attribute.Int("engine.user.count", len(users)))
	return users, total, nil
}

func userListQuery(includeChildren bool) string {
	if includeChildren {
		return userListSQL
	}
	return userListSummarySQL
}

func validateUserListOptions(options *UserListOptions) ([]string, error) {
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 200 || options.Offset < 0 || len(options.Search) > 254 {
		return nil, fmt.Errorf("%w: invalid user list options", ErrInvalidUser)
	}
	statuses := make([]string, len(options.Statuses))
	for index, status := range options.Statuses {
		if err := validateUserStatus(status); err != nil {
			return nil, err
		}
		statuses[index] = string(status)
	}
	return statuses, nil
}

func (s *postgresStore) ListTeamMembers(ctx context.Context, teamID uuid.UUID, options UserListOptions) ([]TeamMember, int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.access.team_members.list")
	defer span.End()
	if teamID == uuid.Nil {
		return nil, 0, ErrTeamNotFound
	}
	statuses, err := validateUserListOptions(&options)
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	batch := &pgx.Batch{}
	batch.Queue(teamMemberCountSQL, teamID, statuses, options.Search)
	batch.Queue(teamMemberListSQL, teamID, statuses, options.Search, options.Limit, options.Offset)
	results := s.db.SendBatch(ctx, batch)
	defer results.Close()
	var total int
	if err := results.QueryRow().Scan(&total); err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("count team members: %w", err))
	}
	rows, err := results.Query()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, fmt.Errorf("list team members: %w", err))
	}
	members, err := scanTeamMembers(rows)
	rows.Close()
	if err != nil {
		return nil, 0, recordTeamSpanError(span, err)
	}
	return members, total, nil
}

func (s *postgresStore) ListUserControlCredentials(ctx context.Context, userID uuid.UUID) ([]ControlCredential, error) {
	rows, err := s.db.Query(ctx, `
		SELECT credential.id, credential.subject_id, credential.key_prefix, credential.name,
			credential.expires_at, credential.last_used_at, credential.revoked_at, credential.created_at
		FROM fused_control_credentials credential
		JOIN fused_users user_row ON user_row.subject_id = credential.subject_id
		WHERE user_row.subject_id = $1 ORDER BY credential.created_at DESC, credential.id
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list user control credentials: %w", err)
	}
	defer rows.Close()
	credentials := make([]ControlCredential, 0)
	for rows.Next() {
		credential, err := scanControlCredential(rows)
		if err != nil {
			return nil, err
		}
		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate user control credentials: %w", err)
	}
	return credentials, nil
}

func (s *postgresStore) GetUserEffectiveAccess(ctx context.Context, userID uuid.UUID) ([]EffectiveAccessGrant, int64, error) {
	rows, err := s.db.Query(ctx, userEffectiveAccessSQL, userID)
	if err != nil {
		return nil, 0, fmt.Errorf("get user effective access: %w", err)
	}
	defer rows.Close()
	grants := make([]EffectiveAccessGrant, 0)
	var revision int64
	found := false
	for rows.Next() {
		var permission, resourceType, roleSlug, sourceType, sourceName *string
		var resourceID, sourceID *uuid.UUID
		if err := rows.Scan(&revision, &permission, &resourceType, &resourceID, &roleSlug, &sourceType, &sourceID, &sourceName); err != nil {
			return nil, 0, fmt.Errorf("scan user effective access: %w", err)
		}
		found = true
		if permission != nil {
			grants = append(grants, EffectiveAccessGrant{
				Permission: accesscontrol.Permission(*permission), Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(*resourceType), ID: *resourceID},
				RoleSlug: *roleSlug, SourceType: *sourceType, SourceID: *sourceID, SourceDisplayName: *sourceName,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate user effective access: %w", err)
	}
	if !found {
		return nil, 0, ErrUserNotFound
	}
	return grants, revision, nil
}

const userOwnerProtectedSQL = `EXISTS (
	SELECT 1
	FROM fused_role_bindings binding
	JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
	JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'account.manage'
	JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
	WHERE binding.resource_type = 'workspace' AND (
		(binding.subject_type = 'subject' AND binding.subject_id = user_row.subject_id)
		OR (binding.subject_type = 'team' AND EXISTS (
			SELECT 1 FROM fused_team_memberships membership
			WHERE membership.team_id = binding.subject_id AND membership.member_subject_id = user_row.subject_id
		))
	)
)`

const userSelectColumnsSQL = `
	SELECT user_row.subject_id, user_row.email_display, subject.display_name, subject.status, ` + userOwnerProtectedSQL + `,
		user_row.created_at, user_row.updated_at,
		COALESCE(membership_rows.items, '[]'::jsonb),
		COALESCE(membership_rows.truncated, false),
		COALESCE(credential_rows.items, '[]'::jsonb),
		COALESCE(credential_rows.truncated, false)
	FROM `

// User pages carry enough nested data for the People UI without requiring
// per-row repository calls. Each lateral subquery reads at most one sentinel
// row beyond the public 100-item cap, so high-cardinality users cannot make a
// page perform unbounded aggregation, and the truncation flags prevent callers
// from presenting a partial nested collection as complete.
const userJoinsSQL = `
	JOIN fused_subjects subject ON subject.id = user_row.subject_id AND subject.kind = 'user'
	LEFT JOIN LATERAL (
		SELECT jsonb_agg(jsonb_build_object(
			'team_id', membership.id, 'team_name', membership.name, 'team_slug', membership.slug,
			'team_status', membership.status, 'role', membership.membership_role,
			'created_at', membership.created_at
		) ORDER BY membership.ordinality) FILTER (WHERE membership.ordinality <= 100) AS items,
		bool_or(membership.ordinality > 100) AS truncated
		FROM (
			SELECT limited.*, row_number() OVER (ORDER BY limited.id) AS ordinality
			FROM (
				SELECT membership.membership_role, membership.created_at,
					team.id, team.name, team.slug, team.status
				FROM fused_team_memberships membership
				JOIN fused_teams team ON team.id = membership.team_id
				WHERE membership.member_subject_id = user_row.subject_id
				ORDER BY team.id LIMIT 101
			) limited
		) membership
	) membership_rows ON true
	LEFT JOIN LATERAL (
		SELECT jsonb_agg(jsonb_build_object(
			'id', credential.id, 'key_prefix', credential.key_prefix, 'name', credential.name,
			'expires_at', credential.expires_at, 'last_used_at', credential.last_used_at,
			'revoked_at', credential.revoked_at, 'created_at', credential.created_at
		) ORDER BY credential.ordinality) FILTER (WHERE credential.ordinality <= 100) AS items,
		bool_or(credential.ordinality > 100) AS truncated
		FROM (
			SELECT limited.*, row_number() OVER (ORDER BY limited.created_at DESC, limited.id) AS ordinality
			FROM (
				SELECT credential.id, credential.key_prefix, credential.name,
					credential.expires_at, credential.last_used_at, credential.revoked_at, credential.created_at
				FROM fused_control_credentials credential
				WHERE credential.subject_id = user_row.subject_id
				ORDER BY credential.created_at DESC, credential.id LIMIT 101
			) limited
		) credential
	) credential_rows ON true
`

const userSelectSQL = userSelectColumnsSQL + `fused_users user_row` + userJoinsSQL

const userFilterSQL = `
	(array_length($1::text[], 1) IS NULL OR subject.status = ANY($1::text[]))
	AND ($2 = '' OR subject.display_name ILIKE '%' || $2 || '%' OR user_row.email_display ILIKE '%' || $2 || '%')
`

const userCountSQL = `SELECT COUNT(*) FROM fused_users user_row JOIN fused_subjects subject ON subject.id = user_row.subject_id WHERE ` + userFilterSQL

const userListSQL = userSelectColumnsSQL + `(
	SELECT user_row.* FROM fused_users user_row
	JOIN fused_subjects subject ON subject.id = user_row.subject_id
	WHERE ` + userFilterSQL + `
	ORDER BY user_row.created_at DESC, user_row.subject_id LIMIT $3 OFFSET $4
) user_row` + userJoinsSQL + ` ORDER BY user_row.created_at DESC, user_row.subject_id`

const userListSummarySQL = `
	SELECT user_row.subject_id, user_row.email_display, subject.display_name, subject.status, ` + userOwnerProtectedSQL + `,
		user_row.created_at, user_row.updated_at,
		'[]'::jsonb, false, '[]'::jsonb, false
	FROM (
		SELECT user_row.* FROM fused_users user_row
		JOIN fused_subjects subject ON subject.id = user_row.subject_id
		WHERE ` + userFilterSQL + `
		ORDER BY user_row.created_at DESC, user_row.subject_id LIMIT $3 OFFSET $4
	) user_row
	JOIN fused_subjects subject ON subject.id = user_row.subject_id AND subject.kind = 'user'
	ORDER BY user_row.created_at DESC, user_row.subject_id`

const teamMemberFilterSQL = `
	membership.team_id = $1
	AND (array_length($2::text[], 1) IS NULL OR subject.status = ANY($2::text[]))
	AND ($3 = '' OR subject.display_name ILIKE '%' || $3 || '%' OR user_row.email_display ILIKE '%' || $3 || '%')
`

const teamMemberCountSQL = `
	SELECT COUNT(*) FROM fused_team_memberships membership
	JOIN fused_users user_row ON user_row.subject_id = membership.member_subject_id
	JOIN fused_subjects subject ON subject.id = user_row.subject_id
	WHERE ` + teamMemberFilterSQL

const teamMemberListSQL = `
	SELECT user_row.subject_id, user_row.email_display, subject.display_name, subject.status,
		membership.membership_role, membership.created_at
	FROM fused_team_memberships membership
	JOIN fused_users user_row ON user_row.subject_id = membership.member_subject_id
	JOIN fused_subjects subject ON subject.id = user_row.subject_id
	WHERE ` + teamMemberFilterSQL + `
	ORDER BY subject.display_name, user_row.subject_id LIMIT $4 OFFSET $5
`

const userEffectiveAccessSQL = `
	WITH target AS (
		SELECT user_row.subject_id, subject.display_name, subject.status, state.revision, workspace.id AS workspace_id
		FROM fused_users user_row
		JOIN fused_subjects subject ON subject.id = user_row.subject_id
		CROSS JOIN fused_authorization_state state
		CROSS JOIN fused_workspaces workspace
		WHERE user_row.subject_id = $1 AND state.singleton_key = 1 AND workspace.singleton_key = 1
	), sources AS (
		SELECT 'subject'::text AS source_type, target.subject_id AS source_id, target.display_name AS source_name
		FROM target WHERE target.status = 'active'
		UNION ALL
		SELECT 'team', team.id, team.name
		FROM target
		JOIN fused_team_memberships membership ON membership.member_subject_id = target.subject_id
		JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
		WHERE target.status = 'active'
	), effective AS (
		SELECT DISTINCT permission.permission, binding.resource_type, binding.resource_id,
			role.slug AS role_slug, source.source_type, source.source_id, source.source_name
		FROM sources source
		JOIN fused_role_bindings binding ON binding.subject_type = source.source_type AND binding.subject_id = source.source_id
		JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = binding.resource_type
		JOIN fused_role_permissions permission ON permission.role_id = role.id
		JOIN target ON binding.resource_type <> 'workspace' OR binding.resource_id = target.workspace_id
	)
	SELECT target.revision, effective.permission, effective.resource_type, effective.resource_id,
		effective.role_slug, effective.source_type, effective.source_id, effective.source_name
	FROM target LEFT JOIN effective ON true
	ORDER BY effective.permission, effective.resource_type, effective.resource_id, effective.role_slug, effective.source_type, effective.source_id
`

func scanUsers(rows pgx.Rows) ([]User, error) {
	users := make([]User, 0)
	for rows.Next() {
		user, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate users: %w", err)
	}
	return users, nil
}

func scanUserRow(row pgx.Row) (User, error) {
	var user User
	var membershipJSON, credentialJSON []byte
	err := row.Scan(&user.ID, &user.Email, &user.DisplayName, &user.Status, &user.OwnerProtected, &user.CreatedAt, &user.UpdatedAt,
		&membershipJSON, &user.MembershipsTruncated, &credentialJSON, &user.CredentialsTruncated)
	if err != nil {
		return User{}, fmt.Errorf("scan user: %w", err)
	}
	if err := json.Unmarshal(membershipJSON, &user.Memberships); err != nil {
		return User{}, fmt.Errorf("scan user memberships: %w", err)
	}
	if err := json.Unmarshal(credentialJSON, &user.Credentials); err != nil {
		return User{}, fmt.Errorf("scan user credentials: %w", err)
	}
	for index := range user.Credentials {
		user.Credentials[index].UserID = user.ID
	}
	return user, nil
}

func scanTeamMembers(rows pgx.Rows) ([]TeamMember, error) {
	members := make([]TeamMember, 0)
	for rows.Next() {
		var member TeamMember
		if err := rows.Scan(&member.UserID, &member.Email, &member.DisplayName, &member.UserStatus, &member.MembershipRole, &member.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan team member: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate team members: %w", err)
	}
	return members, nil
}

func scanControlCredential(row pgx.Row) (ControlCredential, error) {
	var credential ControlCredential
	err := row.Scan(&credential.ID, &credential.UserID, &credential.KeyPrefix, &credential.Name,
		&credential.ExpiresAt, &credential.LastUsedAt, &credential.RevokedAt, &credential.CreatedAt)
	if err != nil {
		return ControlCredential{}, fmt.Errorf("scan control credential: %w", err)
	}
	return credential, nil
}
