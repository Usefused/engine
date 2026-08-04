package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
)

type postgresStore struct {
	db *pgxpool.Pool
}

var ErrWorkspaceOwnerMismatch = errors.New("engine workspace belongs to a different Registry account")

func NewPostgresStore(db *pgxpool.Pool) Store {
	return &postgresStore{db: db}
}

func (s *postgresStore) BootstrapWorkspace(ctx context.Context, accountID uuid.UUID, name string) (uuid.UUID, error) {
	ownerID, err := s.getSingletonWorkspace(ctx)
	if err == nil {
		return s.finishWorkspaceBootstrap(ctx, accountID, ownerID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, fmt.Errorf("load Engine workspace: %w", err)
	}
	if err := s.insertSingletonWorkspace(ctx, accountID, name); err != nil {
		return uuid.Nil, fmt.Errorf("create Engine workspace: %w", err)
	}
	// ON CONFLICT may mean a concurrent startup won the singleton insert.
	// Reloading the owner prevents a process authenticated as another Registry
	// account from accepting the winner's local workspace.
	ownerID, err = s.getSingletonWorkspace(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load created Engine workspace: %w", err)
	}
	return s.finishWorkspaceBootstrap(ctx, accountID, ownerID)
}

func (s *postgresStore) insertSingletonWorkspace(ctx context.Context, accountID uuid.UUID, name string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO fused_workspaces (name, account_id, slug, singleton_key)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (singleton_key) DO NOTHING
	`, name, accountID, accountID.String())
	return err
}

func (s *postgresStore) finishWorkspaceBootstrap(ctx context.Context, accountID, ownerID uuid.UUID) (uuid.UUID, error) {
	if err := validateWorkspaceOwner(accountID, ownerID); err != nil {
		return uuid.Nil, err
	}
	// Ownership must be established before bootstrap performs any local write;
	// a Registry identity for another account must not mutate this singleton
	// workspace as part of a failed startup.
	if err := s.ensureDefaultBucket(ctx); err != nil {
		return uuid.Nil, err
	}
	var workspaceID uuid.UUID
	if err := s.db.QueryRow(ctx, "SELECT id FROM fused_workspaces WHERE singleton_key = 1").Scan(&workspaceID); err != nil {
		return uuid.Nil, fmt.Errorf("fetch workspace ID: %w", err)
	}
	return workspaceID, nil
}

func (s *postgresStore) ensureDefaultBucket(ctx context.Context) error {
	query := `
		INSERT INTO fused_buckets (name, is_default)
		VALUES ('default', true)
		ON CONFLICT (name) DO NOTHING
	`
	_, err := s.db.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("ensure default bucket: %w", err)
	}
	return nil
}

// LoadDefaultBucketID is a point lookup for authorization policy resolution;
// it avoids loading every bucket and filtering in the HTTP layer.
func (s *postgresStore) LoadDefaultBucketID(ctx context.Context) (uuid.UUID, error) {
	var bucketID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT id FROM fused_buckets WHERE is_default = true LIMIT 1`).Scan(&bucketID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load default bucket ID: %w", err)
	}
	return bucketID, nil
}

func (s *postgresStore) getSingletonWorkspace(ctx context.Context) (uuid.UUID, error) {
	var ownerID uuid.UUID
	err := s.db.QueryRow(ctx, `SELECT account_id FROM fused_workspaces WHERE singleton_key = 1`).Scan(&ownerID)
	return ownerID, err
}

func validateWorkspaceOwner(expected, actual uuid.UUID) error {
	if expected == actual {
		return nil
	}
	return fmt.Errorf("%w: workspace owner is %s, license belongs to %s", ErrWorkspaceOwnerMismatch, actual, expected)
}

func (s *postgresStore) GetLatestWorkspaceServiceVersion(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (string, error) {
	err := s.VerifyWorkspaceOwner(ctx, accountID)
	if err != nil {
		return "", fmt.Errorf("workspace not found for account: %w", err)
	}
	version, err := s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, serviceID)
	if err != nil {
		return "", fmt.Errorf("no workspace service version found for service %s: %w", serviceID, err)
	}
	return version, nil
}

func (s *postgresStore) GetLatestWorkspaceServiceVersionID(ctx context.Context, accountID uuid.UUID, serviceID uuid.UUID) (uuid.UUID, error) {
	err := s.VerifyWorkspaceOwner(ctx, accountID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("workspace not found for account: %w", err)
	}
	versionID, err := s.GetLatestWorkspaceServiceVersionIDByWorkspace(ctx, serviceID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("no workspace service version found for service %s: %w", serviceID, err)
	}
	return versionID, nil
}

func (s *postgresStore) SaveArtifactScope(ctx context.Context, scope ArtifactScope) error {
	kind := scope.Kind
	if kind == "" {
		kind = "sdk"
	}
	if !validArtifactOwner(optionalUUID(scope.OwnerSubjectID), optionalUUID(scope.OwnerTeamID)) {
		return ErrArtifactOwnerRequired
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.artifact.scope.persist")
	defer span.End()
	span.SetAttributes(attribute.String("artifact_id", scope.ArtifactID.String()), artifactOwnerSpanAttribute(scope))
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save artifact scope: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return err
	}
	if _, err := saveArtifactScopeTx(ctx, tx, scope, kind); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save artifact scope: commit: %w", err)
	}
	return nil
}

func saveArtifactScopeTx(ctx context.Context, tx pgx.Tx, scope ArtifactScope, kind string) (bool, error) {
	created, err := insertArtifactScopeTx(ctx, tx, scope, kind)
	if err != nil {
		return false, err
	}
	if err := validateArtifactOwner(ctx, tx, scope, created); err != nil {
		return false, err
	}
	if !created {
		if err := updateArtifactScopeTx(ctx, tx, scope, kind); err != nil {
			return false, err
		}
	}
	// Scope insert/update and immutable bucket selection share one transaction.
	if err := validateArtifactBucketAssignment(ctx, tx, scope.ArtifactID, scope.BucketID, created); err != nil {
		return false, err
	}
	if err := linkArtifactBucketTx(ctx, tx, scope.ArtifactID, scope.BucketID); err != nil {
		return false, err
	}
	bindingChanged, err := ensureArtifactOwnerBinding(ctx, tx, scope, scope.ArtifactID)
	if err != nil {
		return false, err
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, bindingChanged)
	if err != nil {
		return false, err
	}
	if err := auditArtifactScopeSave(ctx, tx, scope, revision, created); err != nil {
		return false, err
	}
	return created, nil
}

func insertArtifactScopeTx(ctx context.Context, tx pgx.Tx, scope ArtifactScope, kind string) (bool, error) {
	var created bool
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_artifact_scopes (account_id, artifact_id, owner_subject_id, owner_team_id, scope_schema_version, selections, kind, name, version, config_key)
		VALUES ($1, $2, NULLIF($3, '00000000-0000-0000-0000-000000000000'::uuid), NULLIF($4, '00000000-0000-0000-0000-000000000000'::uuid), $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''))
		ON CONFLICT (artifact_id) DO NOTHING
		RETURNING true
	`, scope.AccountID, scope.ArtifactID, scope.OwnerSubjectID, scope.OwnerTeamID, scope.ScopeSchemaVersion, scope.Selections, kind, scope.Name, scope.Version, scope.ConfigKey).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("insert artifact scope: %w", err)
	}
	return created, nil
}

func updateArtifactScopeTx(ctx context.Context, tx pgx.Tx, scope ArtifactScope, kind string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE fused_artifact_scopes SET
			selections = $10,
			scope_schema_version = $5,
			kind = $6,
			name = COALESCE(NULLIF($7, ''), name),
			version = COALESCE(NULLIF($8, ''), version),
			config_key = COALESCE(NULLIF($9, ''), config_key)
		WHERE artifact_id = $2 AND account_id = $1
		  AND owner_subject_id IS NOT DISTINCT FROM NULLIF($3, '00000000-0000-0000-0000-000000000000'::uuid)
		  AND owner_team_id IS NOT DISTINCT FROM NULLIF($4, '00000000-0000-0000-0000-000000000000'::uuid)
	`, scope.AccountID, scope.ArtifactID, scope.OwnerSubjectID, scope.OwnerTeamID, scope.ScopeSchemaVersion, kind, scope.Name, scope.Version, scope.ConfigKey, scope.Selections)
	if err != nil {
		return fmt.Errorf("update artifact scope: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrArtifactOwnerMismatch
	}
	return nil
}

func validateArtifactBucketAssignment(ctx context.Context, tx pgx.Tx, artifactID, bucketID uuid.UUID, created bool) error {
	var existingBucketID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT bucket_id FROM fused_artifact_buckets WHERE artifact_id = $1 FOR UPDATE`, artifactID).Scan(&existingBucketID)
	if err == nil {
		if bucketID == uuid.Nil || existingBucketID != bucketID {
			return ErrSDKBucketImmutable
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("validate artifact bucket assignment: %w", err)
	}
	// Absence is part of the immutable value for an existing scope. Only a
	// newly created artifact may choose its initial optional bucket assignment.
	if !created {
		if bucketID != uuid.Nil {
			return ErrSDKBucketImmutable
		}
		return nil
	}
	if bucketID == uuid.Nil {
		return nil
	}
	var bucketExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM fused_buckets WHERE id = $1)`, bucketID).Scan(&bucketExists); err != nil {
		return fmt.Errorf("validate artifact bucket: %w", err)
	}
	if !bucketExists {
		return ErrBucketNotFound
	}
	return nil
}

func linkArtifactBucketTx(ctx context.Context, tx pgx.Tx, artifactID, bucketID uuid.UUID) error {
	if bucketID == uuid.Nil {
		return nil
	}
	var linkedBucketID uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO fused_artifact_buckets (artifact_id, bucket_id) VALUES ($1, $2)
		ON CONFLICT (artifact_id) DO UPDATE SET bucket_id = fused_artifact_buckets.bucket_id
		WHERE fused_artifact_buckets.bucket_id = EXCLUDED.bucket_id
		RETURNING bucket_id
	`, artifactID, bucketID).Scan(&linkedBucketID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSDKBucketImmutable
	}
	if err != nil {
		return fmt.Errorf("link artifact bucket: %w", err)
	}
	return nil
}

func validateArtifactOwner(ctx context.Context, tx pgx.Tx, scope ArtifactScope, created bool) error {
	if scope.OwnerSubjectID != uuid.Nil {
		var active bool
		err := tx.QueryRow(ctx, `SELECT status = 'active' FROM fused_subjects WHERE id = $1 FOR UPDATE`, scope.OwnerSubjectID).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			return accesscontrol.ErrAuthenticationRequired
		}
		if err != nil {
			return fmt.Errorf("load artifact owner subject: %w", err)
		}
		// Suspending a person removes their effective grants, but must not brick
		// an existing artifact that a workspace administrator or shared team can
		// still manage. Only initial publication requires an active owner.
		if created && !active {
			return accesscontrol.ErrAuthenticationRequired
		}
		return nil
	}
	var status TeamStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM fused_teams WHERE id = $1 FOR UPDATE`, scope.OwnerTeamID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return ErrTeamNotFound
	} else if err != nil {
		return fmt.Errorf("load artifact owner team: %w", err)
	}
	if status != TeamStatusActive {
		return ErrTeamArchived
	}
	return nil
}

func ensureArtifactOwnerBinding(ctx context.Context, tx pgx.Tx, scope ArtifactScope, artifactID uuid.UUID) (bool, error) {
	subjectType, subjectID := "subject", scope.OwnerSubjectID
	if scope.OwnerTeamID != uuid.Nil {
		subjectType, subjectID = "team", scope.OwnerTeamID
	}
	var inserted, roleExists bool
	err := tx.QueryRow(ctx, `
		WITH role AS (SELECT id FROM fused_roles WHERE slug = $4 AND scope_type = 'artifact'), inserted AS (
			INSERT INTO fused_role_bindings (subject_type, subject_id, role_id, resource_type, resource_id)
			SELECT $1, $2, role.id, 'artifact', $3 FROM role
			ON CONFLICT (subject_type, subject_id, role_id, resource_type, resource_id) DO NOTHING
			RETURNING true
		)
		SELECT COALESCE((SELECT true FROM inserted), false), EXISTS (SELECT 1 FROM role)
	`, subjectType, subjectID, artifactID, accesscontrol.RoleArtifactManager).Scan(&inserted, &roleExists)
	if err != nil {
		return false, fmt.Errorf("ensure artifact owner binding: %w", err)
	}
	if !roleExists {
		return false, errors.New("artifact manager role is unavailable")
	}
	return inserted, nil
}

func artifactOwnerType(scope ArtifactScope) string {
	if scope.OwnerTeamID != uuid.Nil {
		return "team"
	}
	return "subject"
}

func artifactOwnerID(scope ArtifactScope) uuid.UUID {
	if scope.OwnerTeamID != uuid.Nil {
		return scope.OwnerTeamID
	}
	return scope.OwnerSubjectID
}

func artifactOwnerSpanAttribute(scope ArtifactScope) attribute.KeyValue {
	return attribute.String("owner."+artifactOwnerType(scope)+"_id", artifactOwnerID(scope).String())
}

func auditArtifactScopeSave(ctx context.Context, tx pgx.Tx, scope ArtifactScope, revision int64, created bool) error {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	permission := accesscontrol.PermissionArtifactManage
	if created {
		permission = accesscontrol.PermissionArtifactCreate
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, trace_id, outcome, metadata)
		VALUES ($1, $2, 'artifact.scope.persist', $3, 'artifact', $4, $5, 'succeeded',
			jsonb_build_object('owner_type', $6::text, 'owner_id', $7::text,
				'authorization_revision', $8::bigint, 'changed', true))
	`, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID), permission, scope.ArtifactID,
		trace.SpanFromContext(ctx).SpanContext().TraceID().String(), artifactOwnerType(scope), artifactOwnerID(scope).String(), revision)
	if err != nil {
		return fmt.Errorf("audit artifact scope save: %w", err)
	}
	return nil
}

// artifactScopeSelectColumns is shared by GetArtifactScope/ListArtifactScopes/
// ListMCPScopesByAccount so their SELECT lists and Scan order can't drift
// apart from each other.
const artifactScopeSelectColumns = "s.account_id, s.artifact_id, s.owner_subject_id, s.owner_team_id, b.bucket_id, s.scope_schema_version, s.selections, s.deactivated_at, s.kind, s.name, s.version, s.config_key, s.created_at"

func scanArtifactScope(row pgx.Row) (*ArtifactScope, error) {
	var scope ArtifactScope
	var bucketID *uuid.UUID
	var name *string
	var version, configKey *string
	var ownerSubjectID, ownerTeamID *uuid.UUID
	if err := row.Scan(&scope.AccountID, &scope.ArtifactID, &ownerSubjectID, &ownerTeamID, &bucketID, &scope.ScopeSchemaVersion, &scope.Selections, &scope.DeactivatedAt, &scope.Kind, &name, &version, &configKey, &scope.CreatedAt); err != nil {
		return nil, err
	}
	if ownerSubjectID != nil {
		scope.OwnerSubjectID = *ownerSubjectID
	}
	if ownerTeamID != nil {
		scope.OwnerTeamID = *ownerTeamID
	}
	if bucketID != nil {
		scope.BucketID = *bucketID
	}
	if name != nil {
		scope.Name = *name
	}
	if version != nil {
		scope.Version = *version
	}
	if configKey != nil {
		scope.ConfigKey = *configKey
	}
	return &scope, nil
}

func (s *postgresStore) GetArtifactScope(ctx context.Context, artifactID uuid.UUID) (*ArtifactScope, error) {
	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		LEFT JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE s.artifact_id = $1
	`
	scope, err := scanArtifactScope(s.db.QueryRow(ctx, query, artifactID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrArtifactScopeNotFound
		}
		return nil, err
	}
	return scope, nil
}

// DeactivateSDK blocks new MCP session connections for artifactID (enforced in
// LocalObjectCache.loadArtifactScope) without touching selections/bucket
// assignment -- deactivation is reversible via ReactivateSDK and shouldn't
// discard the rest of the scope to achieve that.
func (s *postgresStore) DeactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `UPDATE fused_artifact_scopes SET deactivated_at = NOW() WHERE account_id = $1 AND artifact_id = $2`, accountID, artifactID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrArtifactScopeNotFound
	}
	return nil
}

// ReactivateSDK clears a prior deactivation. Setting deactivated_at = NULL
// unconditionally (rather than only if currently set) is what makes this
// safely idempotent -- a second call against an already-active SDK still
// succeeds instead of needing its own not-deactivated check.
func (s *postgresStore) ReactivateSDK(ctx context.Context, accountID, artifactID uuid.UUID) error {
	tag, err := s.db.Exec(ctx, `UPDATE fused_artifact_scopes SET deactivated_at = NULL WHERE account_id = $1 AND artifact_id = $2`, accountID, artifactID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrArtifactScopeNotFound
	}
	return nil
}

func (s *postgresStore) ListArtifactScopes(ctx context.Context, artifactIDs []uuid.UUID) (map[uuid.UUID]*ArtifactScope, error) {
	out := make(map[uuid.UUID]*ArtifactScope)
	if len(artifactIDs) == 0 {
		return out, nil
	}
	// bucket_id no longer lives on fused_artifact_scopes -- it's derived via
	// fused_artifact_buckets, same as GetArtifactScope above. Mirrored here rather than
	// routing batch reads through GetArtifactScope in a loop, which would turn one
	// query into N (the whole point of ListArtifactScopes existing).
	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		LEFT JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE s.artifact_id = ANY($1::uuid[])
	`
	rows, err := s.db.Query(ctx, query, artifactIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		scope, err := scanArtifactScope(rows)
		if err != nil {
			return nil, err
		}
		out[scope.ArtifactID] = scope
	}
	return out, rows.Err()
}

// ListMCPScopesByAccount is the read side of the MCP servers list page: every
// kind='mcp' scope owned by accountID, newest first, paginated, plus the
// total count (a separate COUNT(*) query rather than window-function trickery
// -- this list is paginated in tens/hundreds of rows per account, not a scale
// where the extra round trip matters).
func (s *postgresStore) ListMCPScopesByAccount(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]ArtifactScope, int, error) {
	return s.ListAuthorizedMCPScopesByAccount(ctx, accountID, accesscontrol.AuthorizedScope{All: true}, limit, offset)
}

func (s *postgresStore) ListAuthorizedMCPScopesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]ArtifactScope, int, error) {
	return s.ListAuthorizedArtifactScopesByAccount(ctx, accountID, scope, "mcp", limit, offset)
}

func (s *postgresStore) ListAuthorizedArtifactScopesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind string, limit, offset int) ([]ArtifactScope, int, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	kind, valid := normalizeArtifactKind(kind)
	if !valid {
		return nil, 0, ErrInvalidArtifactKind
	}
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM fused_artifact_scopes
		WHERE account_id = $1 AND ($2 = '' OR kind = $2)
		  AND ($3 OR artifact_id = ANY($4::uuid[]))`,
		accountID, kind, scope.All, scope.IDs).Scan(&total); err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		LEFT JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE s.account_id = $1 AND ($2 = '' OR s.kind = $2)
		  AND ($3 OR s.artifact_id = ANY($4::uuid[]))
		ORDER BY s.created_at DESC
		LIMIT $5 OFFSET $6
	`
	rows, err := s.db.Query(ctx, query, accountID, kind, scope.All, scope.IDs, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	scopes := make([]ArtifactScope, 0, limit)
	for rows.Next() {
		scope, err := scanArtifactScope(rows)
		if err != nil {
			return nil, 0, err
		}
		scopes = append(scopes, *scope)
	}
	return scopes, total, rows.Err()
}

var _ ArtifactPageRepository = (*postgresStore)(nil)

func (s *postgresStore) GetMCPScopeByName(ctx context.Context, accountID uuid.UUID, name, version string) (*ArtifactScope, error) {
	query := `
		SELECT ` + artifactScopeSelectColumns + `
		FROM fused_artifact_scopes s
		LEFT JOIN fused_artifact_buckets b ON s.artifact_id = b.artifact_id
		WHERE s.account_id = $1 AND s.kind = 'mcp' AND s.name = $2
	`
	args := []interface{}{accountID, name}
	if version != "" {
		query += ` AND s.version = $3`
		args = append(args, version)
	}
	query += ` ORDER BY s.created_at DESC LIMIT 1`

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	if !rows.Next() {
		return nil, ErrArtifactScopeNotFound
	}
	return scanArtifactScope(rows)
}

func (s *postgresStore) DeleteArtifactScope(ctx context.Context, accountID uuid.UUID, artifactID uuid.UUID) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("delete artifact scope: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := lockAuthorizationState(ctx, tx); err != nil {
		return err
	}
	if err := deleteArtifactScopeTx(ctx, tx, accountID, artifactID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func deleteArtifactScopeTx(ctx context.Context, tx pgx.Tx, accountID, artifactID uuid.UUID) error {
	var configKey *string
	err := tx.QueryRow(ctx, `SELECT config_key FROM fused_artifact_scopes WHERE account_id = $1 AND artifact_id = $2 FOR UPDATE`, accountID, artifactID).Scan(&configKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("delete artifact scope: lock: %w", err)
	}
	if err := deleteArtifactConfigRefsTx(ctx, tx, artifactID, configKey); err != nil {
		return err
	}
	return deleteArtifactRowsTx(ctx, tx, accountID, artifactID)
}

func deleteArtifactConfigRefsTx(ctx context.Context, tx pgx.Tx, artifactID uuid.UUID, configKey *string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_config_states WHERE latest_resource_id = $1`, artifactID); err != nil {
		return fmt.Errorf("delete artifact config state: %w", err)
	}
	if configKey != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE fused_config_plans SET status = 'superseded', superseded_at = NOW()
			WHERE config_key = $1 AND status = 'pending'
		`, *configKey); err != nil {
			return fmt.Errorf("supersede artifact config plans: %w", err)
		}
	}
	return nil
}

func deleteArtifactRowsTx(ctx context.Context, tx pgx.Tx, accountID, artifactID uuid.UUID) error {
	bindingTag, err := tx.Exec(ctx, `DELETE FROM fused_role_bindings WHERE resource_type = 'artifact' AND resource_id = $1`, artifactID)
	if err != nil {
		return fmt.Errorf("delete artifact bindings: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fused_artifact_scopes WHERE account_id = $1 AND artifact_id = $2`, accountID, artifactID); err != nil {
		return fmt.Errorf("delete artifact scope: %w", err)
	}
	revision, err := bumpAuthorizationRevision(ctx, tx, bindingTag.RowsAffected() > 0)
	if err != nil {
		return err
	}
	actor, _ := accesscontrol.ActorFromContext(ctx)
	if _, err := tx.Exec(ctx, `
		INSERT INTO fused_audit_events (actor_subject_id, actor_credential_id, action, permission,
			resource_type, resource_id, trace_id, outcome, metadata)
		VALUES ($1, $2, 'artifact.delete', $3, 'artifact', $4, $5, 'succeeded',
			jsonb_build_object('authorization_revision', $6::bigint, 'changed', true))
	`, nullableUUID(actor.SubjectID), nullableUUID(actor.CredentialID), accesscontrol.PermissionArtifactManage,
		artifactID, trace.SpanFromContext(ctx).SpanContext().TraceID().String(), revision); err != nil {
		return fmt.Errorf("audit artifact delete: %w", err)
	}
	return nil
}

func (s *postgresStore) GetSDKAccountID(ctx context.Context, artifactID uuid.UUID) (uuid.UUID, error) {
	var accountID uuid.UUID
	err := s.db.QueryRow(ctx, "SELECT account_id FROM fused_artifact_scopes WHERE artifact_id = $1", artifactID).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, errors.New("sdk not found")
		}
		return uuid.Nil, fmt.Errorf("query sdk account error: %w", err)
	}
	return accountID, nil
}

func (s *postgresStore) ValidateToken(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error) {
	query := `
		WITH updated_sdk_token AS (
			UPDATE fused_artifact_tokens
			SET last_used_at = NOW()
			WHERE token_hash = $2 AND artifact_id = $1
			RETURNING artifact_id
		)
		SELECT s.account_id
		FROM fused_artifact_scopes s
		WHERE s.artifact_id = $1
		AND EXISTS (SELECT 1 FROM updated_sdk_token)
	`
	var accountID uuid.UUID
	err := s.db.QueryRow(ctx, query, artifactID, tokenHash).Scan(&accountID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, errors.New("unauthorized")
		}
		return uuid.Nil, fmt.Errorf("validate token error: %w", err)
	}
	return accountID, nil
}

// VerifyWorkspaceOwner returns nil if the Engine's singleton workspace
// belongs to the authenticated Registry account.
func (s *postgresStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	ownerID, err := s.getSingletonWorkspace(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errors.New("Engine workspace is not initialized")
		}
		return fmt.Errorf("VerifyWorkspaceOwner: %w", err)
	}
	if err := validateWorkspaceOwner(accountID, ownerID); err != nil {
		return err
	}
	return nil
}

func (s *postgresStore) UpsertMCPSession(ctx context.Context, session *models.MCPSession) error {
	query := `
		INSERT INTO fused_mcp_sessions (id, artifact_id, session_id, started_at, ended_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			ended_at = EXCLUDED.ended_at
	`
	_, err := s.db.Exec(ctx, query, session.ID, session.ArtifactID, session.SessionID, session.StartedAt, session.EndedAt)
	return err
}

// GetMCPAnalyticsDashboard uses SQL aggregation over canonical execution
// events. Session lifecycle remains a separate concern because a connection
// is not an execution and has different retention and update semantics.
func (s *postgresStore) GetMCPAnalyticsDashboard(ctx context.Context, artifactID uuid.UUID) (*models.MCPAnalyticsDashboard, error) {
	dashboard := &models.MCPAnalyticsDashboard{}

	totalsQuery := `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(AVG(latency_ms), 0)
		FROM fused_engine_execution_events WHERE artifact_id = $1 AND transport = 'mcp'
	`
	if err := s.db.QueryRow(ctx, totalsQuery, artifactID).Scan(&dashboard.TotalRequests, &dashboard.FailedRequests, &dashboard.AverageLatencyMs); err != nil {
		return nil, fmt.Errorf("query mcp analytics totals: %w", err)
	}

	activeQuery := `SELECT COUNT(*) FROM fused_mcp_sessions WHERE artifact_id = $1 AND ended_at IS NULL`
	if err := s.db.QueryRow(ctx, activeQuery, artifactID).Scan(&dashboard.ActiveAgents); err != nil {
		return nil, fmt.Errorf("query mcp active agents: %w", err)
	}

	toolUsage, err := queryMCPToolUsage(ctx, s.db, artifactID)
	if err != nil {
		return nil, err
	}
	dashboard.ToolUsage = toolUsage

	serviceUsage, err := queryMCPServiceUsage(ctx, s.db, artifactID)
	if err != nil {
		return nil, err
	}
	dashboard.ServiceUsage = serviceUsage

	recentSessions, err := queryRecentMCPSessions(ctx, s.db, artifactID)
	if err != nil {
		return nil, err
	}
	dashboard.RecentSessions = recentSessions

	return dashboard, nil
}

func queryMCPToolUsage(ctx context.Context, db *pgxpool.Pool, artifactID uuid.UUID) ([]models.MCPToolUsage, error) {
	query := `
		SELECT endpoint_name, COUNT(*), COUNT(*) FILTER (WHERE status = 'failed'), COALESCE(AVG(latency_ms), 0)
		FROM fused_engine_execution_events
		WHERE artifact_id = $1 AND transport = 'mcp' AND endpoint_name <> ''
		GROUP BY endpoint_name
		ORDER BY COUNT(*) DESC
	`
	rows, err := db.Query(ctx, query, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query mcp tool usage: %w", err)
	}
	defer rows.Close()

	var usage []models.MCPToolUsage
	for rows.Next() {
		var u models.MCPToolUsage
		if err := rows.Scan(&u.ToolName, &u.Count, &u.Failed, &u.AverageLatencyMs); err != nil {
			return nil, fmt.Errorf("scan mcp tool usage: %w", err)
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

func queryMCPServiceUsage(ctx context.Context, db *pgxpool.Pool, artifactID uuid.UUID) ([]models.MCPServiceUsage, error) {
	query := `
		SELECT COALESCE(workspace_service.service_name, event.service_id::text), COUNT(*),
			COUNT(*) FILTER (WHERE event.status = 'failed'), COALESCE(AVG(event.latency_ms), 0)
		FROM fused_engine_execution_events event
		LEFT JOIN fused_workspace_services workspace_service ON workspace_service.service_id = event.service_id
		WHERE event.artifact_id = $1 AND event.transport = 'mcp' AND event.service_id IS NOT NULL
		GROUP BY COALESCE(workspace_service.service_name, event.service_id::text)
		ORDER BY COUNT(*) DESC
	`
	rows, err := db.Query(ctx, query, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query mcp service usage: %w", err)
	}
	defer rows.Close()

	var usage []models.MCPServiceUsage
	for rows.Next() {
		var u models.MCPServiceUsage
		if err := rows.Scan(&u.ServiceName, &u.Count, &u.Failed, &u.AverageLatencyMs); err != nil {
			return nil, fmt.Errorf("scan mcp service usage: %w", err)
		}
		usage = append(usage, u)
	}
	return usage, rows.Err()
}

// queryRecentMCPSessions caps at 10 rows -- this backs a UI summary panel,
// not a full session history browser.
func queryRecentMCPSessions(ctx context.Context, db *pgxpool.Pool, artifactID uuid.UUID) ([]models.MCPSession, error) {
	query := `
		SELECT id, artifact_id, session_id, started_at, ended_at
		FROM fused_mcp_sessions
		WHERE artifact_id = $1
		ORDER BY started_at DESC
		LIMIT 10
	`
	rows, err := db.Query(ctx, query, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query recent mcp sessions: %w", err)
	}
	defer rows.Close()

	var sessions []models.MCPSession
	for rows.Next() {
		var sess models.MCPSession
		if err := rows.Scan(&sess.ID, &sess.ArtifactID, &sess.SessionID, &sess.StartedAt, &sess.EndedAt); err != nil {
			return nil, fmt.Errorf("scan mcp session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

func (s *postgresStore) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	if len(events) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	query := `
		INSERT INTO fused_engine_execution_events (
			id, trace_id, span_id, account_id, artifact_id, transport, direction, service_id, service_version_id,
			operation_id, webhook_id, endpoint_name, external_id, event_name, http_method, request_path,
			environment, environment_source, provider_host, provider_http_status, provider_status_class,
			status, failure_reason, failure_category, failure_code, latency_ms, provider_latency_ms,
			attempt_count, request_bytes, response_bytes, verification_status, delivery_status,
			idempotency_key_hash, request_body_hash, idempotency_replayed, timings, started_at, ended_at, created_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20,
			$21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			failure_reason = EXCLUDED.failure_reason,
			failure_category = EXCLUDED.failure_category,
			failure_code = EXCLUDED.failure_code,
			latency_ms = EXCLUDED.latency_ms,
			attempt_count = EXCLUDED.attempt_count,
			request_bytes = EXCLUDED.request_bytes,
			response_bytes = EXCLUDED.response_bytes,
			verification_status = EXCLUDED.verification_status,
			delivery_status = EXCLUDED.delivery_status,
			ended_at = EXCLUDED.ended_at
	`
	for _, event := range events {
		b.Queue(query,
			event.ID, event.TraceID, event.SpanID, nullableUUID(event.AccountID), nullableUUID(event.ArtifactID), event.Transport,
			event.Direction, nullableUUID(event.ServiceID), event.ServiceVersionID, nullableUUID(event.OperationID), nullableUUID(event.WebhookID),
			event.EndpointName, event.ExternalID, event.EventName, event.HTTPMethod, event.RequestPath, event.Environment,
			event.EnvironmentSource, event.ProviderHost, event.ProviderHTTPStatus, event.ProviderStatusClass, event.Status,
			event.FailureReason, event.FailureCategory, event.FailureCode, event.LatencyMs, event.ProviderLatencyMs,
			event.AttemptCount, event.RequestBytes, event.ResponseBytes, event.VerificationStatus, event.DeliveryStatus,
			event.IdempotencyKeyHash, event.RequestBodyHash, event.IdempotencyReplayed, event.Timings,
			event.StartedAt, event.EndedAt, event.CreatedAt,
		)
	}
	results := s.db.SendBatch(ctx, b)
	return results.Close()
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func (s *postgresStore) DeleteEngineExecutionEventsBefore(ctx context.Context, before time.Time, limit int) (int64, error) {
	result, err := s.db.Exec(ctx, `
		WITH expired AS (
			SELECT id FROM fused_engine_execution_events
			WHERE started_at < $1
			ORDER BY started_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		DELETE FROM fused_engine_execution_events event
		USING expired
		WHERE event.id = expired.id`, before, limit)
	if err != nil {
		return 0, fmt.Errorf("delete expired execution events: %w", err)
	}
	return result.RowsAffected(), nil
}

type EngineExecutionFilter struct {
	AccountID  uuid.UUID
	ServiceID  uuid.UUID
	ArtifactID uuid.UUID
	Transport  string
	Direction  string
	Status     string
	Limit      int
	Offset     int
	StartDate  *time.Time
	EndDate    *time.Time
}

// ArtifactExecutionEventReader is optional so alternate Store implementations
// can adopt artifact activity without widening the core persistence contract.
type ArtifactExecutionEventReader interface {
	ListEngineExecutionEventsByArtifact(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error)
}

type ArtifactExecutionAnalyticsReader interface {
	GetEngineExecutionAnalyticsByArtifact(ctx context.Context, filter EngineExecutionFilter) (models.ArtifactExecutionAnalytics, error)
}

func engineExecutionWhereClause(filter EngineExecutionFilter) (string, []any) {
	scopeColumn, scopeID := "service_id", filter.ServiceID
	if filter.ArtifactID != uuid.Nil {
		scopeColumn, scopeID = "artifact_id", filter.ArtifactID
	}
	// Keeping tenant and resource scope in the same SQL predicate prevents a
	// caller from receiving broad workspace data and filtering it in memory.
	whereClause := fmt.Sprintf("WHERE account_id = $1 AND %s = $2", scopeColumn)
	args := []any{filter.AccountID, scopeID}
	argIdx := 3
	if filter.Transport != "" {
		whereClause += fmt.Sprintf(" AND transport = $%d", argIdx)
		args = append(args, filter.Transport)
		argIdx++
	}
	if filter.Direction != "" {
		whereClause += fmt.Sprintf(" AND direction = $%d", argIdx)
		args = append(args, filter.Direction)
		argIdx++
	}
	if filter.Status != "" {
		whereClause += fmt.Sprintf(" AND status = $%d", argIdx)
		args = append(args, filter.Status)
		argIdx++
	}
	if filter.StartDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *filter.StartDate)
		argIdx++
	}
	if filter.EndDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *filter.EndDate)
	}
	return whereClause, args
}

func (s *postgresStore) ListEngineExecutionEventsByService(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filter.ArtifactID = uuid.Nil
	return s.listEngineExecutionEvents(ctx, filter)
}

func (s *postgresStore) ListEngineExecutionEventsByArtifact(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filter.ServiceID = uuid.Nil
	return s.listEngineExecutionEvents(ctx, filter)
}

func (s *postgresStore) listEngineExecutionEvents(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	var count int64
	if err := s.db.QueryRow(ctx, "SELECT COUNT(*) FROM fused_engine_execution_events "+whereClause, args...).Scan(&count); err != nil {
		return nil, 0, err
	}

	argIdx := len(args) + 1
	query := `SELECT id, COALESCE(trace_id, ''), COALESCE(span_id, ''), COALESCE(account_id, '00000000-0000-0000-0000-000000000000'::uuid),
		COALESCE(artifact_id, '00000000-0000-0000-0000-000000000000'::uuid), transport, direction, service_id, COALESCE(service_version_id, ''),
		COALESCE(operation_id, '00000000-0000-0000-0000-000000000000'::uuid), COALESCE(webhook_id, '00000000-0000-0000-0000-000000000000'::uuid),
		endpoint_name, COALESCE(external_id, ''), COALESCE(event_name, ''), COALESCE(http_method, ''), COALESCE(request_path, ''), COALESCE(environment, ''),
		COALESCE(environment_source, ''), COALESCE(provider_host, ''), provider_http_status, COALESCE(provider_status_class, ''),
		status, COALESCE(failure_reason, ''), COALESCE(failure_category, ''), COALESCE(failure_code, ''), latency_ms, provider_latency_ms,
		attempt_count, request_bytes, response_bytes, COALESCE(verification_status, ''), COALESCE(delivery_status, ''),
		idempotency_replayed, COALESCE(timings, '{}'::jsonb), started_at, ended_at, created_at
		FROM fused_engine_execution_events ` + whereClause + fmt.Sprintf(" ORDER BY started_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, filter.Limit, filter.Offset)
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	events := make([]models.EngineExecutionEvent, 0, filter.Limit)
	for rows.Next() {
		var event models.EngineExecutionEvent
		if err := rows.Scan(
			&event.ID, &event.TraceID, &event.SpanID, &event.AccountID, &event.ArtifactID, &event.Transport, &event.Direction,
			&event.ServiceID, &event.ServiceVersionID, &event.OperationID, &event.WebhookID, &event.EndpointName,
			&event.ExternalID, &event.EventName, &event.HTTPMethod, &event.RequestPath, &event.Environment, &event.EnvironmentSource,
			&event.ProviderHost, &event.ProviderHTTPStatus, &event.ProviderStatusClass, &event.Status, &event.FailureReason,
			&event.FailureCategory, &event.FailureCode, &event.LatencyMs, &event.ProviderLatencyMs, &event.AttemptCount,
			&event.RequestBytes, &event.ResponseBytes, &event.VerificationStatus, &event.DeliveryStatus,
			&event.IdempotencyReplayed, &event.Timings, &event.StartedAt, &event.EndedAt, &event.CreatedAt,
		); err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return events, count, nil
}

func (s *postgresStore) GetEngineExecutionAnalyticsByService(ctx context.Context, filter EngineExecutionFilter) (models.EngineExecutionAnalytics, error) {
	filter.ArtifactID = uuid.Nil
	return s.getEngineExecutionAnalytics(ctx, filter)
}

func (s *postgresStore) GetEngineExecutionAnalyticsByArtifact(ctx context.Context, filter EngineExecutionFilter) (models.ArtifactExecutionAnalytics, error) {
	filter.ServiceID = uuid.Nil
	summary, err := s.getEngineExecutionAnalytics(ctx, filter)
	if err != nil {
		return models.ArtifactExecutionAnalytics{}, err
	}
	byService, err := s.getArtifactServiceExecutionBreakdown(ctx, filter)
	if err != nil {
		return models.ArtifactExecutionAnalytics{}, err
	}
	return models.ArtifactExecutionAnalytics{EngineExecutionAnalytics: summary, ByService: byService}, nil
}

func (s *postgresStore) getEngineExecutionAnalytics(ctx context.Context, filter EngineExecutionFilter) (models.EngineExecutionAnalytics, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	query := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE status = 'success'),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COALESCE(AVG(latency_ms), 0),
		COALESCE(percentile_cont(0.5) WITHIN GROUP (ORDER BY latency_ms), 0),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM fused_engine_execution_events ` + whereClause
	var analytics models.EngineExecutionAnalytics
	err := s.db.QueryRow(ctx, query, args...).Scan(
		&analytics.TotalCalls,
		&analytics.SuccessfulCalls,
		&analytics.FailedCalls,
		&analytics.AverageLatencyMs,
		&analytics.MedianLatencyMs,
		&analytics.P95LatencyMs,
	)
	return analytics, err
}

func (s *postgresStore) getArtifactServiceExecutionBreakdown(ctx context.Context, filter EngineExecutionFilter) ([]models.EngineExecutionBreakdown, error) {
	whereClause, args := engineExecutionWhereClause(filter)
	// Keeping grouping in SQL avoids loading individual receipts just to count
	// them in Go and makes the query count independent of bundled service count.
	query := `SELECT service_id::text, service_id::text, COUNT(*),
		COUNT(*) FILTER (WHERE status = 'failed'),
		COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
		FROM fused_engine_execution_events ` + whereClause + `
		GROUP BY service_id ORDER BY COUNT(*) DESC`
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]models.EngineExecutionBreakdown, 0)
	for rows.Next() {
		var item models.EngineExecutionBreakdown
		if err := rows.Scan(&item.Key, &item.Label, &item.TotalCalls, &item.FailedCalls, &item.P95LatencyMs); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (s *postgresStore) ListWebhookEventsByService(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, limit, offset int, startDate, endDate *time.Time) ([]models.WebhookEvent, int64, error) {
	whereClause := "WHERE account_id = $1 AND service_id = $2 AND transport = 'webhook'"
	args := []any{accountID, serviceID}
	argIdx := 3

	if eventName != "" {
		whereClause += fmt.Sprintf(" AND event_name = $%d", argIdx)
		args = append(args, eventName)
		argIdx++
	}
	if startDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *startDate)
		argIdx++
	}
	if endDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *endDate)
		argIdx++
	}

	countQuery := "SELECT COUNT(*) FROM fused_engine_execution_events " + whereClause
	var count int64
	if err := s.db.QueryRow(ctx, countQuery, args...).Scan(&count); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, account_id, service_id, COALESCE(external_id, ''), COALESCE(event_name, ''),
		COALESCE(failure_reason, ''), artifact_id, COALESCE(verification_status, ''), COALESCE(delivery_status, ''),
		COALESCE(environment, ''), latency_ms::integer, GREATEST(attempt_count - 1, 0), 0::double precision,
		request_bytes::integer, started_at FROM fused_engine_execution_events ` + whereClause + fmt.Sprintf(" ORDER BY started_at DESC LIMIT $%d OFFSET $%d", argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	events, err := pgx.CollectRows(rows, pgx.RowToStructByNameLax[models.WebhookEvent])
	if err != nil {
		return nil, 0, err
	}
	return events, count, nil
}

func (s *postgresStore) GetWebhookAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) (models.WebhookAnalytics, error) {
	whereClause := "WHERE account_id = $1 AND service_id = $2 AND transport = 'webhook'"
	args := []any{accountID, serviceID}
	argIdx := 3

	if eventName != "" {
		whereClause += fmt.Sprintf(" AND event_name = $%d", argIdx)
		args = append(args, eventName)
		argIdx++
	}
	if startDate != nil {
		whereClause += fmt.Sprintf(" AND started_at >= $%d", argIdx)
		args = append(args, *startDate)
		argIdx++
	}
	if endDate != nil {
		whereClause += fmt.Sprintf(" AND started_at <= $%d", argIdx)
		args = append(args, *endDate)
		argIdx++
	}

	var analytics models.WebhookAnalytics
	query := `SELECT COUNT(*),
		COUNT(*) FILTER (WHERE delivery_status IN ('success', 'delivered')),
		COUNT(*) FILTER (WHERE delivery_status = 'rejected'),
		COUNT(*) FILTER (WHERE delivery_status = 'failed')
		FROM fused_engine_execution_events ` + whereClause
	err := s.db.QueryRow(ctx, query, args...).Scan(
		&analytics.TotalIngested, &analytics.TotalDelivered, &analytics.TotalRejected, &analytics.TotalFailed,
	)
	return analytics, err
}

// GetIdempotentExecution looks up a cached response for (artifactID,
// idempotencyKeyHash). expires_at > NOW() makes TTL expiry a plain read-time
// filter -- expired rows just stop matching, no sweep job required for
// correctness (though one could trim storage over time as a follow-up).
func (s *postgresStore) GetIdempotentExecution(ctx context.Context, artifactID uuid.UUID, idempotencyKeyHash, requestBodyHash string) (*models.IdempotentExecution, error) {
	query := `
		SELECT id, artifact_id, idempotency_key_hash, request_body_hash, environment, response_body, response_status, created_at, expires_at
		FROM fused_engine_idempotency_keys
		WHERE artifact_id = $1 AND idempotency_key_hash = $2 AND expires_at > NOW()
	`
	var exec models.IdempotentExecution
	err := s.db.QueryRow(ctx, query, artifactID, idempotencyKeyHash).Scan(
		&exec.ID, &exec.ArtifactID, &exec.IdempotencyKeyHash, &exec.RequestBodyHash,
		&exec.Environment, &exec.ResponseBody, &exec.ResponseStatus, &exec.CreatedAt, &exec.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrIdempotentExecutionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get idempotent execution: %w", err)
	}
	if requestBodyHash != "" && exec.RequestBodyHash != "" && exec.RequestBodyHash != requestBodyHash {
		return nil, ErrIdempotencyKeyConflict
	}
	return &exec, nil
}

// SaveIdempotentExecution caches a successful execution's response.
// ON CONFLICT DO NOTHING: if two concurrent requests with the same key both
// reach here, the first write wins and the second is a harmless no-op --
// both callers already have their (equivalent) response in hand from their
// own dispatch, this table only serves future lookups.
func (s *postgresStore) SaveIdempotentExecution(ctx context.Context, exec *models.IdempotentExecution) error {
	query := `
		INSERT INTO fused_engine_idempotency_keys
			(id, artifact_id, idempotency_key_hash, request_body_hash, environment, response_body, response_status, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), $8)
		ON CONFLICT (artifact_id, idempotency_key_hash) DO NOTHING
	`
	_, err := s.db.Exec(ctx, query, exec.ArtifactID, exec.IdempotencyKeyHash, exec.RequestBodyHash, exec.Environment, exec.ResponseBody, exec.ResponseStatus, exec.ExpiresAt)
	return err
}

func (s *postgresStore) GetArtifactByToken(ctx context.Context, tokenHash string) (*ArtifactScope, error) {
	query := `
		UPDATE fused_artifact_tokens
		SET last_used_at = NOW()
		WHERE token_hash = $1
		RETURNING artifact_id
	`
	var artifactID uuid.UUID
	err := s.db.QueryRow(ctx, query, tokenHash).Scan(&artifactID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrArtifactScopeNotFound
		}
		return nil, err
	}
	return s.GetArtifactScope(ctx, artifactID)
}

func (s *postgresStore) CreateSDKToken(ctx context.Context, artifactID uuid.UUID, tokenHash, name string) (*SDKToken, error) {
	query := `
		INSERT INTO fused_artifact_tokens (artifact_id, token_hash, name)
		VALUES ($1, $2, $3)
		RETURNING id, artifact_id, token_hash, name, last_used_at, created_at
	`
	var token SDKToken
	err := s.db.QueryRow(ctx, query, artifactID, tokenHash, name).Scan(
		&token.ID, &token.ArtifactID, &token.TokenHash, &token.Name, &token.LastUsedAt, &token.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &token, nil
}

func (s *postgresStore) ListSDKTokens(ctx context.Context, artifactID uuid.UUID) ([]SDKToken, error) {
	query := `
		SELECT id, artifact_id, token_hash, name, last_used_at, created_at
		FROM fused_artifact_tokens
		WHERE artifact_id = $1
		ORDER BY created_at ASC
	`
	rows, err := s.db.Query(ctx, query, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []SDKToken
	for rows.Next() {
		var token SDKToken
		if err := rows.Scan(&token.ID, &token.ArtifactID, &token.TokenHash, &token.Name, &token.LastUsedAt, &token.CreatedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (s *postgresStore) RevokeSDKToken(ctx context.Context, artifactID uuid.UUID, name string) error {
	query := `DELETE FROM fused_artifact_tokens WHERE artifact_id = $1 AND name = $2`
	_, err := s.db.Exec(ctx, query, artifactID, name)
	return err
}

func (s *postgresStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	query := `
		DELETE FROM fused_workspace_secrets 
		WHERE bucket_id = $1 
		AND service_id = $2 
		AND key_name = $3 
	`
	_, err := s.db.Exec(ctx, query, bucketID, serviceID, keyName)
	return err
}

func (s *postgresStore) ListSecretMeta(ctx context.Context, bucketID uuid.UUID) ([]WorkspaceSecretMeta, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1
	`
	rows, err := s.db.Query(ctx, query, bucketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectSecretMetas(rows)
}

func (s *postgresStore) ListSecretMetaPage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]WorkspaceSecretMeta, int, error) {
	const credentialRows = `
		SELECT *, CASE
			WHEN LOWER(REPLACE(credential_type, '-', '_')) = 'basic' THEN REGEXP_REPLACE(key_name, '_(username|password)$', '')
			WHEN LOWER(REPLACE(credential_type, '-', '_')) IN ('mtls', 'mutualtls', 'mutual_tls') THEN REGEXP_REPLACE(key_name, '_(cert|key)$', '')
			ELSE key_name
		END AS family_key
		FROM fused_workspace_secrets WHERE bucket_id = $1`
	var total int
	countQuery := `SELECT COUNT(*) FROM (
		SELECT service_id, family_key, credential_type FROM (` + credentialRows + `) rows
		GROUP BY service_id, family_key, credential_type
	) credential_families`
	if err := s.db.QueryRow(ctx, countQuery, bucketID).Scan(&total); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	query := `
		WITH credential_rows AS (` + credentialRows + `)
		SELECT (ARRAY_AGG(id ORDER BY id))[1],  bucket_id, service_id, family_key,
		       credential_type, MAX(last_used_at), MIN(expires_at), MIN(created_at), MAX(updated_at),
		       ARRAY_AGG(key_name ORDER BY key_name)
		FROM credential_rows
		GROUP BY  bucket_id, service_id, family_key, credential_type
		ORDER BY MAX(updated_at) DESC, family_key ASC
		LIMIT $2 OFFSET $3
	`
	rows, err := s.db.Query(ctx, query, bucketID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	metas, err := collectSecretCredentialMetas(rows)
	return metas, total, err
}

func collectSecretCredentialMetas(rows pgx.Rows) ([]WorkspaceSecretMeta, error) {
	var metas []WorkspaceSecretMeta
	for rows.Next() {
		var meta WorkspaceSecretMeta
		if err := rows.Scan(
			&meta.ID, &meta.BucketID, &meta.ServiceID, &meta.KeyName, &meta.CredentialType,
			&meta.LastUsedAt, &meta.ExpiresAt, &meta.CreatedAt, &meta.UpdatedAt, &meta.KeyNames,
		); err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func collectSecretMetas(rows pgx.Rows) ([]WorkspaceSecretMeta, error) {
	var metas []WorkspaceSecretMeta
	for rows.Next() {
		var meta WorkspaceSecretMeta
		if err := rows.Scan(
			&meta.ID, &meta.BucketID, &meta.ServiceID, &meta.KeyName, &meta.CredentialType,
			&meta.LastUsedAt, &meta.ExpiresAt, &meta.CreatedAt, &meta.UpdatedAt,
		); err != nil {
			return nil, err
		}
		metas = append(metas, meta)
	}
	return metas, rows.Err()
}

func (s *postgresStore) ListSecretsForBucket(ctx context.Context, bucketID, serviceID uuid.UUID) ([]WorkspaceSecret, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2
	`
	rows, err := s.db.Query(ctx, query, bucketID, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

func (s *postgresStore) GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*WorkspaceSecret, error) {
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2 AND key_name = $3
	`
	var sec WorkspaceSecret
	err := s.db.QueryRow(ctx, query, bucketID, serviceID, keyName).Scan(
		&sec.ID, &sec.BucketID, &sec.ServiceID, &sec.KeyName, &sec.CredentialType,
		&sec.EncryptedDEK, &sec.EncryptedValue, &sec.LastUsedAt, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sec, nil
}

func (s *postgresStore) GetSecrets(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]WorkspaceSecret, error) {
	keyNames = uniqueSecretKeyNames(keyNames)
	if len(keyNames) == 0 {
		return nil, nil
	}
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = $1 AND service_id = $2 AND key_name = ANY($3)
	`
	rows, err := s.db.Query(ctx, query, bucketID, serviceID, keyNames)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

// uniqueSecretKeyNames keeps exact-key queries compact without changing the
// caller's security boundary: every returned row still matches bucket+service.
func uniqueSecretKeyNames(keyNames []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(keyNames))
	for _, keyName := range keyNames {
		if keyName == "" || seen[keyName] {
			continue
		}
		seen[keyName] = true
		out = append(out, keyName)
	}
	return out
}

func (s *postgresStore) ListSecretsForBuckets(ctx context.Context, bucketIDs []uuid.UUID, serviceID uuid.UUID) ([]WorkspaceSecret, error) {
	if len(bucketIDs) == 0 {
		return nil, nil
	}
	query := `
		SELECT id,  bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, last_used_at, expires_at, created_at, updated_at
		FROM fused_workspace_secrets
		WHERE bucket_id = ANY($1) AND service_id = $2
	`
	rows, err := s.db.Query(ctx, query, bucketIDs, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanWorkspaceSecrets(rows)
}

type workspaceSecretRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanWorkspaceSecrets(rows workspaceSecretRows) ([]WorkspaceSecret, error) {
	var secrets []WorkspaceSecret
	for rows.Next() {
		sec, err := scanWorkspaceSecret(rows)
		if err != nil {
			return nil, err
		}
		secrets = append(secrets, sec)
	}
	return secrets, rows.Err()
}

func scanWorkspaceSecret(rows workspaceSecretRows) (WorkspaceSecret, error) {
	var sec WorkspaceSecret
	err := rows.Scan(
		&sec.ID, &sec.BucketID, &sec.ServiceID,
		&sec.KeyName, &sec.CredentialType, &sec.EncryptedDEK, &sec.EncryptedValue,
		&sec.LastUsedAt, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt,
	)
	return sec, err
}
