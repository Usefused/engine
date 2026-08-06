package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) LoadControlPrincipal(ctx context.Context, credentialHash string) (accesscontrol.ControlPrincipal, error) {
	query := `
		WITH candidate AS (
			SELECT w.id AS workspace_id, w.account_id, c.id AS credential_id, c.subject_id, c.expires_at,
				c.source, c.auth_method,
				s.kind, s.display_name, COALESCE(user_row.email_display, '') AS email_display, state.revision
			FROM fused_control_credentials c
			JOIN fused_subjects s ON s.id = c.subject_id
			LEFT JOIN fused_users user_row ON user_row.subject_id = s.id
			JOIN fused_workspaces w ON w.singleton_key = 1
			JOIN fused_authorization_state state ON state.singleton_key = 1
			WHERE c.key_hash = $1
				AND c.revoked_at IS NULL
				AND (c.expires_at IS NULL OR c.expires_at > NOW())
				AND s.status = 'active'
		), authenticated AS (
			UPDATE fused_control_credentials credential
			SET last_used_at = NOW()
			FROM candidate
			WHERE credential.id = candidate.credential_id
			RETURNING candidate.workspace_id, candidate.account_id, candidate.credential_id,
				candidate.subject_id, candidate.display_name, candidate.email_display,
				candidate.expires_at, candidate.source, candidate.auth_method,
				candidate.kind, candidate.revision
		), principals(subject_type, subject_id) AS (
			SELECT 'subject'::text, subject_id FROM authenticated
			UNION ALL
			SELECT 'team'::text, membership.team_id
			FROM authenticated actor
			JOIN fused_team_memberships membership ON membership.member_subject_id = actor.subject_id
			JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
			UNION ALL
			-- Workspace shares are another principal so cached authorization still
			-- resolves the complete permission set in one database round trip.
			SELECT 'workspace'::text, workspace_id FROM authenticated
		), effective_grants AS (
			SELECT DISTINCT permission.permission, binding.resource_type, binding.resource_id
			FROM principals principal
			JOIN fused_role_bindings binding
				ON binding.subject_type = principal.subject_type
				AND binding.subject_id = principal.subject_id
			JOIN fused_roles role
				ON role.id = binding.role_id AND role.scope_type = binding.resource_type
			JOIN fused_role_permissions permission ON permission.role_id = role.id
			WHERE binding.resource_type <> 'workspace'
				OR binding.resource_id = (SELECT workspace_id FROM authenticated)
		)
		SELECT actor.account_id, actor.workspace_id, actor.subject_id, actor.display_name,
			actor.email_display, actor.credential_id, actor.kind, actor.expires_at,
			actor.source, actor.auth_method, actor.revision,
			effective.permission, effective.resource_type,
			effective.resource_id
		FROM authenticated actor
		LEFT JOIN effective_grants effective ON true
		ORDER BY effective.permission, effective.resource_type, effective.resource_id
	`
	rows, err := s.db.Query(ctx, query, credentialHash)
	if err != nil {
		return accesscontrol.ControlPrincipal{}, fmt.Errorf("load control principal: %w", err)
	}
	defer rows.Close()

	principal, found, err := scanControlPrincipal(rows)
	if err != nil {
		return accesscontrol.ControlPrincipal{}, err
	}
	if !found {
		return accesscontrol.ControlPrincipal{}, accesscontrol.ErrAuthenticationRequired
	}
	return principal, nil
}

func (s *postgresStore) LoadAuthorizationRevision(ctx context.Context) (int64, error) {
	var revision int64
	if err := s.db.QueryRow(ctx, `
		SELECT revision FROM fused_authorization_state WHERE singleton_key = 1
	`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("load authorization revision: %w", err)
	}
	return revision, nil
}

// ResolveAuthorizationResourceDisplayNames resolves only the pre-authorized
// resource IDs supplied by the caller in one query. It is used for safe denial
// UX and never broad-lists resources or changes the authorization decision.
func (s *postgresStore) ResolveAuthorizationResourceDisplayNames(ctx context.Context, resources []accesscontrol.ResourceRef) (map[accesscontrol.ResourceRef]string, error) {
	types := make([]string, len(resources))
	ids := make([]uuid.UUID, len(resources))
	for i, resource := range resources {
		types[i], ids[i] = string(resource.Type), resource.ID
	}
	rows, err := s.db.Query(ctx, `
		WITH requested AS (
			SELECT resource_type, resource_id
			FROM unnest($1::text[], $2::uuid[]) AS input(resource_type, resource_id)
		)
		SELECT requested.resource_type, requested.resource_id,
			CASE requested.resource_type
				WHEN 'workspace' THEN workspace.name
				WHEN 'service' THEN service.service_name
				WHEN 'bucket' THEN bucket.name
				WHEN 'app' THEN app.display_name
			END
		FROM requested
		LEFT JOIN fused_workspaces workspace ON requested.resource_type = 'workspace' AND workspace.id = requested.resource_id
		LEFT JOIN fused_workspace_services service ON requested.resource_type = 'service' AND service.service_id = requested.resource_id
		LEFT JOIN fused_buckets bucket ON requested.resource_type = 'bucket' AND bucket.id = requested.resource_id
		LEFT JOIN fused_app_families app ON requested.resource_type = 'app' AND app.app_family_id = requested.resource_id
	`, types, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve authorization display names: %w", err)
	}
	defer rows.Close()
	resolved := make(map[accesscontrol.ResourceRef]string, len(resources))
	for rows.Next() {
		var resourceType string
		var resourceID uuid.UUID
		var displayName *string
		if err := rows.Scan(&resourceType, &resourceID, &displayName); err != nil {
			return nil, fmt.Errorf("scan authorization display name: %w", err)
		}
		if displayName != nil && *displayName != "" {
			resolved[accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(resourceType), ID: resourceID}] = *displayName
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate authorization display names: %w", err)
	}
	return resolved, nil
}

func (s *postgresStore) RecordAuthorizationAudit(ctx context.Context, event accesscontrol.AuditEvent) error {
	if err := event.Validate(); err != nil {
		return fmt.Errorf("validate authorization audit: %w", err)
	}
	metadata, err := accesscontrol.SanitizeAuditMetadata(event.Metadata)
	if err != nil {
		return fmt.Errorf("sanitize authorization audit: %w", err)
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode authorization audit metadata: %w", err)
	}
	missingRequirements, err := accesscontrol.MarshalRequiredPermissions(event.MissingRequirements)
	if err != nil {
		return fmt.Errorf("encode missing audit requirements: %w", err)
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.ID == uuid.Nil {
		event.ID = uuid.New()
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO fused_audit_events (
			id, occurred_at, actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, request_id, trace_id, method, path, outcome,
			status_code, reason_code, source_ip, user_agent, missing_requirements, metadata
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)
		ON CONFLICT (id) DO NOTHING
	`, event.ID, event.OccurredAt, nullableUUID(event.ActorSubjectID), nullableUUID(event.ActorCredentialID), event.Action,
		nullableAuditText(string(event.Permission)), nullableAuditText(string(event.Resource.Type)), nullableUUID(event.Resource.ID),
		event.RequestID, event.TraceID, event.Method, event.Path, event.Outcome, event.StatusCode, event.ReasonCode,
		event.SourceIP, event.UserAgent, missingRequirements, encodedMetadata)
	if err != nil {
		return fmt.Errorf("record authorization audit: %w", err)
	}
	return nil
}

func nullableAuditText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanControlPrincipal(rows pgx.Rows) (accesscontrol.ControlPrincipal, bool, error) {
	var principal accesscontrol.ControlPrincipal
	found := false
	for rows.Next() {
		var kind string
		var expiresAt *time.Time
		var permission, resourceType *string
		var resourceID *uuid.UUID
		if err := rows.Scan(
			&principal.AccountID,
			&principal.WorkspaceID,
			&principal.SubjectID,
			&principal.DisplayName,
			&principal.Email,
			&principal.CredentialID,
			&kind,
			&expiresAt,
			&principal.CredentialSource,
			&principal.AuthenticationMethod,
			&principal.Revision,
			&permission,
			&resourceType,
			&resourceID,
		); err != nil {
			return accesscontrol.ControlPrincipal{}, false, fmt.Errorf("scan control principal: %w", err)
		}
		found = true
		principal.Kind = accesscontrol.SubjectKind(kind)
		principal.ExpiresAt = expiresAt
		if permission != nil && resourceType != nil && resourceID != nil {
			principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
				Permission: accesscontrol.Permission(*permission),
				Resource: accesscontrol.ResourceRef{
					Type: accesscontrol.ResourceType(*resourceType),
					ID:   *resourceID,
				},
			})
		}
	}
	if err := rows.Err(); err != nil {
		return accesscontrol.ControlPrincipal{}, false, fmt.Errorf("iterate control grants: %w", err)
	}
	return principal, found, nil
}

func (s *postgresStore) ReconcileBootstrapOwner(ctx context.Context, input accesscontrol.BootstrapInput) (accesscontrol.BootstrapResult, error) {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return accesscontrol.BootstrapResult{}, fmt.Errorf("begin access bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	result, err := reconcileBootstrapOwnerTx(ctx, tx, input)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return accesscontrol.BootstrapResult{}, fmt.Errorf("commit access bootstrap: %w", err)
	}
	return result, nil
}

func reconcileBootstrapOwnerTx(ctx context.Context, tx pgx.Tx, input accesscontrol.BootstrapInput) (accesscontrol.BootstrapResult, error) {
	workspaceID, err := bootstrapWorkspaceID(ctx, tx, input.AccountID)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	roleIDs, rolesChanged, err := seedSystemRoles(ctx, tx, input.Roles)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	permissionsChanged, err := reconcileSystemRolePermissions(ctx, tx, input.Roles)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	subjectID, subjectChanged, err := upsertBootstrapSubject(ctx, tx)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	credentialID, credentialChanged, err := rotateBootstrapCredential(ctx, tx, subjectID, input)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	bindingChanged, err := ensureOwnerBinding(ctx, tx, subjectID, subjectID, workspaceID, roleIDs[accesscontrol.RoleOwner])
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	managedOwnerChanged, err := reconcileManagedOwnerInvitation(
		ctx, tx, input.OwnerEmail, subjectID, workspaceID, roleIDs[accesscontrol.RoleOwner],
	)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}

	changed := anyAccessChange(rolesChanged, permissionsChanged, subjectChanged, credentialChanged, bindingChanged, managedOwnerChanged)
	revision, err := finalizeAccessBootstrap(ctx, tx, input.TraceID, workspaceID, subjectID, credentialID, changed)
	if err != nil {
		return accesscontrol.BootstrapResult{}, err
	}
	return accesscontrol.BootstrapResult{
		WorkspaceID:  workspaceID,
		SubjectID:    subjectID,
		CredentialID: credentialID,
		Revision:     revision,
		Changed:      changed,
	}, nil
}

func reconcileManagedOwnerInvitation(ctx context.Context, tx pgx.Tx, email string, bootstrapSubjectID, workspaceID, ownerRoleID uuid.UUID) (bool, error) {
	if strings.TrimSpace(email) == "" {
		return false, nil
	}
	normalized, display, err := normalizeUserEmail(email)
	if err != nil {
		return false, fmt.Errorf("validate managed owner email: %w", err)
	}
	// Registry owns the initial owner email, while Engine owns authorization.
	// Seeding an invitation here bridges those boundaries without allowing an
	// arbitrary Logto identity to create itself or gain local access.
	displayName := strings.SplitN(display, "@", 2)[0]
	user, created, err := loadOrCreateInvitedUser(ctx, tx, normalized, display, displayName)
	if err != nil {
		return false, fmt.Errorf("reconcile managed owner invitation: %w", err)
	}
	if user.Status != UserStatusInvited && user.Status != UserStatusActive {
		return false, fmt.Errorf("reconcile managed owner invitation: owner is %s", user.Status)
	}
	bindingChanged, err := ensureOwnerBinding(ctx, tx, user.ID, bootstrapSubjectID, workspaceID, ownerRoleID)
	if err != nil {
		return false, err
	}
	return created || bindingChanged, nil
}

func anyAccessChange(changes ...bool) bool {
	for _, changed := range changes {
		if changed {
			return true
		}
	}
	return false
}

func bootstrapWorkspaceID(ctx context.Context, tx pgx.Tx, accountID uuid.UUID) (uuid.UUID, error) {
	var workspaceID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM fused_workspaces WHERE singleton_key = 1 AND account_id = $1`, accountID).Scan(&workspaceID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load bootstrap workspace: %w", err)
	}
	return workspaceID, nil
}

func seedSystemRoles(ctx context.Context, tx pgx.Tx, roles []accesscontrol.RoleDefinition) (map[string]uuid.UUID, bool, error) {
	slugs, names, scopes := roleSeedColumns(roles)
	query := `
		WITH desired(slug, display_name, scope_type) AS (
			SELECT * FROM unnest($1::text[], $2::text[], $3::text[])
		), existing AS (
			SELECT r.id, r.slug, r.display_name, r.scope_type, r.system_role
			FROM fused_roles r JOIN desired d ON d.slug = r.slug
		), upserted AS (
			INSERT INTO fused_roles (slug, display_name, scope_type, system_role)
			SELECT slug, display_name, scope_type, true FROM desired
			ON CONFLICT (slug) DO UPDATE SET
				display_name = EXCLUDED.display_name,
				scope_type = EXCLUDED.scope_type,
				system_role = true
			WHERE (fused_roles.display_name, fused_roles.scope_type, fused_roles.system_role)
				IS DISTINCT FROM (EXCLUDED.display_name, EXCLUDED.scope_type, true)
			RETURNING id, slug
		)
		SELECT id, slug, true FROM upserted
		UNION ALL
		SELECT e.id, e.slug, false FROM existing e
		WHERE NOT EXISTS (SELECT 1 FROM upserted u WHERE u.slug = e.slug)
	`
	rows, err := tx.Query(ctx, query, slugs, names, scopes)
	if err != nil {
		return nil, false, fmt.Errorf("seed system roles: %w", err)
	}
	defer rows.Close()

	roleIDs := make(map[string]uuid.UUID, len(roles))
	changed := false
	for rows.Next() {
		var id uuid.UUID
		var slug string
		var rowChanged bool
		if err := rows.Scan(&id, &slug, &rowChanged); err != nil {
			return nil, false, fmt.Errorf("scan system role: %w", err)
		}
		roleIDs[slug] = id
		changed = changed || rowChanged
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate system roles: %w", err)
	}
	if len(roleIDs) != len(roles) || roleIDs[accesscontrol.RoleOwner] == uuid.Nil {
		return nil, false, errors.New("system role seed returned an incomplete role set")
	}
	return roleIDs, changed, nil
}

func roleSeedColumns(roles []accesscontrol.RoleDefinition) ([]string, []string, []string) {
	slugs := make([]string, 0, len(roles))
	names := make([]string, 0, len(roles))
	scopes := make([]string, 0, len(roles))
	for _, role := range roles {
		slugs = append(slugs, role.Slug)
		names = append(names, role.DisplayName)
		scopes = append(scopes, string(role.ScopeType))
	}
	return slugs, names, scopes
}

func reconcileSystemRolePermissions(ctx context.Context, tx pgx.Tx, roles []accesscontrol.RoleDefinition) (bool, error) {
	roleSlugs, permissions := permissionSeedColumns(roles)
	query := `
		WITH desired(role_slug, permission) AS (
			SELECT * FROM unnest($1::text[], $2::text[])
		), target_roles AS (
			SELECT id, slug FROM fused_roles WHERE slug = ANY($3::text[]) AND system_role = true
		), deleted AS (
			DELETE FROM fused_role_permissions rp USING target_roles tr
			WHERE rp.role_id = tr.id
			AND NOT EXISTS (
				SELECT 1 FROM desired d
				WHERE d.role_slug = tr.slug AND d.permission = rp.permission
			)
			RETURNING rp.role_id
		), inserted AS (
			INSERT INTO fused_role_permissions (role_id, permission)
			SELECT tr.id, d.permission
			FROM desired d JOIN target_roles tr ON tr.slug = d.role_slug
			ON CONFLICT (role_id, permission) DO NOTHING
			RETURNING role_id
		)
		SELECT (SELECT COUNT(*) FROM deleted), (SELECT COUNT(*) FROM inserted)
	`
	allRoleSlugs, _, _ := roleSeedColumns(roles)
	var deleted, inserted int
	if err := tx.QueryRow(ctx, query, roleSlugs, permissions, allRoleSlugs).Scan(&deleted, &inserted); err != nil {
		return false, fmt.Errorf("reconcile system role permissions: %w", err)
	}
	return deleted > 0 || inserted > 0, nil
}

func permissionSeedColumns(roles []accesscontrol.RoleDefinition) ([]string, []string) {
	roleSlugs := make([]string, 0)
	permissions := make([]string, 0)
	for _, role := range roles {
		for _, permission := range role.Permissions {
			roleSlugs = append(roleSlugs, role.Slug)
			permissions = append(permissions, string(permission))
		}
	}
	return roleSlugs, permissions
}

func upsertBootstrapSubject(ctx context.Context, tx pgx.Tx) (uuid.UUID, bool, error) {
	query := `
		WITH existing AS (
			SELECT id FROM fused_subjects WHERE kind = 'bootstrap'
		), upserted AS (
			INSERT INTO fused_subjects (kind, display_name, status)
			VALUES ('bootstrap', 'Workspace Owner', 'active')
			ON CONFLICT (kind) WHERE kind = 'bootstrap' DO UPDATE SET
				display_name = EXCLUDED.display_name,
				status = EXCLUDED.status,
				updated_at = NOW()
			WHERE (fused_subjects.display_name, fused_subjects.status)
				IS DISTINCT FROM (EXCLUDED.display_name, EXCLUDED.status)
			RETURNING id
		)
		SELECT id, true FROM upserted
		UNION ALL
		SELECT id, false FROM existing
		WHERE NOT EXISTS (SELECT 1 FROM upserted)
		LIMIT 1
	`
	var subjectID uuid.UUID
	var changed bool
	if err := tx.QueryRow(ctx, query).Scan(&subjectID, &changed); err != nil {
		return uuid.Nil, false, fmt.Errorf("upsert bootstrap subject: %w", err)
	}
	return subjectID, changed, nil
}

func rotateBootstrapCredential(ctx context.Context, tx pgx.Tx, subjectID uuid.UUID, input accesscontrol.BootstrapInput) (uuid.UUID, bool, error) {
	revoked, err := tx.Exec(ctx, `
		UPDATE fused_control_credentials
		SET revoked_at = NOW()
		WHERE subject_id = $1 AND revoked_at IS NULL AND key_hash <> $2
	`, subjectID, input.CredentialHash)
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("revoke prior bootstrap credential: %w", err)
	}

	query := `
		WITH existing AS (
			SELECT id FROM fused_control_credentials WHERE key_hash = $2
		), upserted AS (
			INSERT INTO fused_control_credentials (subject_id, key_hash, key_prefix, name)
			VALUES ($1, $2, $3, 'FUSED_LICENSE_KEY')
			ON CONFLICT (key_hash) DO UPDATE SET
				subject_id = EXCLUDED.subject_id,
				key_prefix = EXCLUDED.key_prefix,
				name = EXCLUDED.name,
				expires_at = NULL,
				revoked_at = NULL
			WHERE (fused_control_credentials.subject_id, fused_control_credentials.key_prefix,
				fused_control_credentials.name, fused_control_credentials.expires_at,
				fused_control_credentials.revoked_at)
				IS DISTINCT FROM (EXCLUDED.subject_id, EXCLUDED.key_prefix, EXCLUDED.name, NULL, NULL)
			RETURNING id
		)
		SELECT id, true FROM upserted
		UNION ALL
		SELECT id, false FROM existing
		WHERE NOT EXISTS (SELECT 1 FROM upserted)
		LIMIT 1
	`
	var credentialID uuid.UUID
	var upsertChanged bool
	if err := tx.QueryRow(ctx, query, subjectID, input.CredentialHash, input.CredentialPrefix).Scan(&credentialID, &upsertChanged); err != nil {
		return uuid.Nil, false, fmt.Errorf("upsert bootstrap credential: %w", err)
	}
	return credentialID, revoked.RowsAffected() > 0 || upsertChanged, nil
}

func ensureOwnerBinding(ctx context.Context, tx pgx.Tx, subjectID, createdBySubjectID, workspaceID, ownerRoleID uuid.UUID) (bool, error) {
	if ownerRoleID == uuid.Nil {
		return false, errors.New("owner role ID is required")
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO fused_role_bindings (
			subject_type, subject_id, role_id, resource_type, resource_id, created_by_subject_id
		) VALUES ('subject', $1, $2, 'workspace', $3, $4)
		ON CONFLICT (subject_type, subject_id, role_id, resource_type, resource_id) DO UPDATE
		SET created_by_subject_id = EXCLUDED.created_by_subject_id
		WHERE fused_role_bindings.created_by_subject_id IS DISTINCT FROM EXCLUDED.created_by_subject_id
	`, subjectID, ownerRoleID, workspaceID, createdBySubjectID)
	if err != nil {
		return false, fmt.Errorf("ensure bootstrap Owner binding: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

func finalizeAccessBootstrap(ctx context.Context, tx pgx.Tx, traceID string, workspaceID, subjectID, credentialID uuid.UUID, changed bool) (int64, error) {
	var revision int64
	query := `SELECT revision FROM fused_authorization_state WHERE singleton_key = 1`
	if changed {
		query = `UPDATE fused_authorization_state SET revision = revision + 1, updated_at = NOW() WHERE singleton_key = 1 RETURNING revision`
	}
	if err := tx.QueryRow(ctx, query).Scan(&revision); err != nil {
		return 0, fmt.Errorf("load authorization revision: %w", err)
	}
	if !changed {
		return revision, nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (
			actor_subject_id, actor_credential_id, action, permission, resource_type,
			resource_id, trace_id, outcome, metadata
		) VALUES ($1, $2, 'access.bootstrap_owner', 'access.manage', 'workspace', $3, $4,
			'succeeded', jsonb_build_object('authorization_revision', $5::bigint))
	`, subjectID, credentialID, workspaceID, traceID, revision)
	if err != nil {
		return 0, fmt.Errorf("audit access bootstrap: %w", err)
	}
	return revision, nil
}
