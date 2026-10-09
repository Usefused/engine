package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// These arrays are currently exposed by non-paginated GraphQL fields. A hard
// database limit keeps one request from loading an unbounded family history or
// token set while preserving ample room for normal version and token usage.
const appFamilyCollectionLimit = 500

// --- AppFamily CRUD ---

// CreateOrGetAppFamily reserves one live family identity without reviving archived history.
func (s *postgresStore) CreateOrGetAppFamily(ctx context.Context, family AppFamily) (*AppFamily, bool, error) {
	if !family.Kind.Valid() {
		return nil, false, ErrAppKindInvalid
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app_family.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.family_id", family.AppFamilyID.String()),
		attribute.String("app.kind", family.Kind.String()),
	)

	return createOrGetAppFamily(ctx, s.db, family)
}

type appFamilyQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// createOrGetAppFamily is shared by lifecycle reservation and atomic config
// apply. Both operations need identical conflict identity and scan semantics,
// while their callers retain responsibility for transaction ownership.
func createOrGetAppFamily(ctx context.Context, queryer appFamilyQueryer, family AppFamily) (*AppFamily, bool, error) {
	row := queryer.QueryRow(ctx, `
		INSERT INTO fused_app_families AS family
			(app_family_id, account_id, kind, canonical_name, display_name,
			 target_language, delivery_mode, owner_subject_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''),
		        NULLIF($7, ''),
		        NULLIF($8, '00000000-0000-0000-0000-000000000000'::uuid),
		        NULLIF($9, '00000000-0000-0000-0000-000000000000'::uuid))
		ON CONFLICT (account_id, kind, canonical_name) WHERE archived_at IS NULL DO UPDATE
		SET delivery_mode = COALESCE(family.delivery_mode, EXCLUDED.delivery_mode),
		    updated_at = family.updated_at
		RETURNING app_family_id, account_id, kind, canonical_name, display_name,
		          COALESCE(target_language, ''), COALESCE(delivery_mode, ''),
		          COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		          COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
		          archived_at,
		          COALESCE(archived_by_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		          created_at, updated_at, (xmax = 0)
	`, family.AppFamilyID, family.AccountID, family.Kind, family.CanonicalName,
		family.DisplayName, family.TargetLanguage, family.DeliveryMode, family.OwnerSubjectID, family.OwnerTeamID)
	var result AppFamily
	var created bool
	err := row.Scan(&result.AppFamilyID, &result.AccountID, &result.Kind,
		&result.CanonicalName, &result.DisplayName, &result.TargetLanguage,
		&result.DeliveryMode,
		&result.OwnerSubjectID, &result.OwnerTeamID, &result.ArchivedAt, &result.ArchivedBy, &result.CreatedAt,
		&result.UpdatedAt, &created)
	if err != nil {
		return nil, false, fmt.Errorf("create or get app family: %w", err)
	}
	return &result, created, nil
}

const appFamilySelect = `
SELECT app_family_id, account_id, kind, canonical_name, display_name,
       COALESCE(target_language, ''), COALESCE(delivery_mode, ''),
       COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
       COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
	   archived_at,
	   COALESCE(archived_by_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
       created_at, updated_at
FROM fused_app_families`

// GetAppFamily resolves only a live family so archived identities cannot regain runtime state.
func (s *postgresStore) GetAppFamily(ctx context.Context, appFamilyID uuid.UUID) (*AppFamily, error) {
	row := s.db.QueryRow(ctx, appFamilySelect+` WHERE app_family_id = $1 AND archived_at IS NULL`, appFamilyID)
	f, err := scanAppFamily(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppFamilyNotFound
	}
	return f, err
}

// GetAppFamilyByIdentity resolves the one reusable live name and ignores retained archived identities.
func (s *postgresStore) GetAppFamilyByIdentity(ctx context.Context, accountID uuid.UUID, kind, canonicalName string) (*AppFamily, error) {
	row := s.db.QueryRow(ctx, appFamilySelect+`
		WHERE account_id = $1 AND kind = $2 AND canonical_name = $3 AND archived_at IS NULL`,
		accountID, kind, canonicalName)
	f, err := scanAppFamily(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppFamilyNotFound
	}
	return f, err
}

// AppFamilyHasHistory reports whether an otherwise-unbound family has any live or retired immutable version identity.
func (s *postgresStore) AppFamilyHasHistory(ctx context.Context, appFamilyID uuid.UUID) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM fused_apps WHERE app_family_id = $1
			UNION ALL
			SELECT 1 FROM fused_app_tombstones WHERE app_family_id = $1
		)
	`, appFamilyID).Scan(&exists)
	// Storage failures cannot prove a reservation shell is safe to bind.
	if err != nil {
		return false, fmt.Errorf("check app family history: %w", err)
	}
	return exists, nil
}

// ListAppFamilies returns only live families; historical identities use the archive catalogue instead.
func (s *postgresStore) ListAppFamilies(ctx context.Context, accountID uuid.UUID, kind string, limit, offset int) ([]AppFamily, int, error) {
	rows, err := s.db.Query(ctx, `
		SELECT app_family_id, account_id, kind, canonical_name, display_name,
		       COALESCE(target_language, ''), COALESCE(delivery_mode, ''),
		       COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       archived_at,
		       COALESCE(archived_by_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       created_at, updated_at, COUNT(*) OVER()
		FROM fused_app_families
		WHERE account_id = $1 AND archived_at IS NULL AND ($2 = '' OR kind = $2)
		ORDER BY kind, canonical_name
		LIMIT $3 OFFSET $4
	`, accountID, kind, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list app families: %w", err)
	}
	defer rows.Close()

	var families []AppFamily
	var total int
	for rows.Next() {
		var f AppFamily
		if err := rows.Scan(&f.AppFamilyID, &f.AccountID, &f.Kind, &f.CanonicalName,
			&f.DisplayName, &f.TargetLanguage, &f.DeliveryMode, &f.OwnerSubjectID, &f.OwnerTeamID,
			&f.ArchivedAt, &f.ArchivedBy, &f.CreatedAt, &f.UpdatedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("scan app family: %w", err)
		}
		families = append(families, f)
	}
	return families, total, rows.Err()
}

// scanAppFamily decodes the complete live-or-historical family projection in one stable column order.
func scanAppFamily(row pgx.Row) (*AppFamily, error) {
	var f AppFamily
	err := row.Scan(&f.AppFamilyID, &f.AccountID, &f.Kind, &f.CanonicalName,
		&f.DisplayName, &f.TargetLanguage, &f.DeliveryMode, &f.OwnerSubjectID, &f.OwnerTeamID,
		&f.ArchivedAt, &f.ArchivedBy, &f.CreatedAt, &f.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// GetAppFamilyQuotaUsage counts runnable families and reports whether the
// target already occupies capacity in the same bounded SQL statement.
func (s *postgresStore) GetAppFamilyQuotaUsage(ctx context.Context, accountID uuid.UUID, kind, canonicalName string) (AppFamilyQuotaUsage, error) {
	var usage AppFamilyQuotaUsage
	err := s.db.QueryRow(ctx, `
		WITH scoped_families AS (
			SELECT family.canonical_name, family.kind,
			       EXISTS (
				 SELECT 1
				 FROM fused_apps app
				 WHERE app.app_family_id = family.app_family_id
				   AND app.account_id = family.account_id
				   AND app.status IN ('active', 'deprecated')
			   AND ($2 NOT IN ('mcp', 'hosted_mcp') OR family.kind = 'mcp' OR app.hosted_mcp)
			   AND (
			     $2 NOT IN ('api', 'sdk')
				     OR ($2 = 'api' AND app.sdk_generation_status = 'skipped')
				     OR ($2 = 'sdk' AND app.sdk_generation_status IS DISTINCT FROM 'skipped')
				   )
			       ) AS invokable
			FROM fused_app_families family
			WHERE family.account_id = $1
			  AND family.archived_at IS NULL
			  AND (
			    $2 = ''
		    OR ($2 = 'api' AND family.kind = 'sdk')
		    OR ($2 IN ('mcp', 'hosted_mcp') AND family.kind IN ('mcp', 'sdk', 'unified_app'))
		    OR ($2 NOT IN ('api', 'mcp', 'hosted_mcp') AND family.kind = $2)
			  )
		)
		SELECT COUNT(*) FILTER (WHERE invokable),
		       COALESCE(BOOL_OR(canonical_name = $3 AND invokable AND (
		         ($2 = 'hosted_mcp' AND kind IN ('sdk', 'unified_app')) OR
		         ($2 <> 'hosted_mcp' AND ($2 <> 'mcp' OR kind = 'mcp'))
		       )), FALSE)
		FROM scoped_families
	`, accountID, kind, canonicalName).Scan(&usage.CurrentInvokable, &usage.TargetInvokable)
	return usage, err
}

// --- App (version) CRUD ---

func (s *postgresStore) PublishAppVersion(ctx context.Context, app App) (*App, bool, error) {
	if !app.Status.Valid() {
		return nil, false, ErrAppStatusInvalid
	}
	if !app.ExpectedFamilyKind.Valid() {
		return nil, false, ErrAppKindInvalid
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.create")
	defer span.End()
	span.SetAttributes(
		attribute.String("app.id", app.AppID.String()),
		attribute.String("app.family_id", app.AppFamilyID.String()),
		attribute.String("app.version", app.Version),
	)

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("publish app: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	persisted, created, err := publishAppVersionTx(ctx, tx, app)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("publish app: commit: %w", err)
	}
	return persisted, created, nil
}

// publishAppVersionTx is the single persistence implementation for immutable
// app publication. Standalone lifecycle operations and atomic config apply both
// use it so tombstone, immutability, and capability semantics cannot drift.
func publishAppVersionTx(ctx context.Context, tx pgx.Tx, app App) (*App, bool, error) {
	if err := lockAppFamily(ctx, tx, app); err != nil {
		return nil, false, err
	}
	existing, err := loadVersionForPublish(ctx, tx, app.AppFamilyID, app.Version)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		return publishExistingAppVersionTx(ctx, tx, *existing, app)
	}
	return publishNewAppVersionTx(ctx, tx, app)
}

// publishExistingAppVersionTx verifies immutable identity before a retry may promote a stable route.
func publishExistingAppVersionTx(ctx context.Context, tx pgx.Tx, existing, requested App) (*App, bool, error) {
	// An older MCP declaration can be reapplied, but it may not replace code or provider scope.
	if !sameImmutableAppVersion(existing, requested) {
		return nil, false, ErrAppVersionImmutable
	}
	// A failed SDK build may become active later; only a runnable version owns the stable alias now.
	if existing.Status.Runnable() {
		if err := promoteStableMCPVersionTx(ctx, tx, requested); err != nil {
			return nil, false, err
		}
	}
	return &existing, false, nil
}

// publishNewAppVersionTx keeps tombstone, scope, and route publication inside one transaction.
func publishNewAppVersionTx(ctx context.Context, tx pgx.Tx, app App) (*App, bool, error) {
	if err := rejectTombstonedVersion(ctx, tx, app.AppFamilyID, app.Version); err != nil {
		return nil, false, err
	}
	if err := insertApp(ctx, tx, app); err != nil {
		return nil, false, err
	}
	if err := insertAppCapabilities(ctx, tx, app.AppID, app.CapabilityKeys); err != nil {
		return nil, false, err
	}
	if err := promoteStableMCPVersionTx(ctx, tx, app); err != nil {
		return nil, false, err
	}
	return &app, true, nil
}

// promoteStableMCPVersionTx advances the shared family's MCP alias only for a runnable MCP delivery.
func promoteStableMCPVersionTx(ctx context.Context, tx pgx.Tx, app App) error {
	// A Unified App's stable MCP alias moves only when its ready bundle takes traffic.
	if app.ExpectedFamilyKind == AppKindUnifiedApp {
		return nil
	}
	// Plain SDK versions cannot acquire an MCP route through an unrelated apply.
	if (app.ExpectedFamilyKind != AppKindMCP && !app.HostedMCP) || !app.Status.Runnable() {
		return nil
	}
	result, err := tx.Exec(ctx, `
		UPDATE fused_app_families family
		SET mcp_stable_app_id = app.app_id,
		    mcp_stable_route_initialized = true,
		    updated_at = CASE
		      WHEN family.mcp_stable_app_id IS DISTINCT FROM app.app_id THEN NOW()
		      ELSE family.updated_at
		    END
		FROM fused_apps app
		WHERE family.app_family_id = $1
		  AND (family.kind = 'mcp' OR (family.kind IN ('sdk', 'unified_app') AND app.hosted_mcp))
		  AND app.app_id = $2
		  AND app.app_family_id = family.app_family_id
		  AND app.status IN ('active', 'deprecated')
	`, app.AppFamilyID, app.AppID)
	// A failed promotion must abort the same transaction that published or
	// reapplied the immutable version.
	if err != nil {
		return fmt.Errorf("promote stable MCP version: %w", err)
	}
	// Publication must fail atomically if the version cannot satisfy the stable
	// route invariant; silently leaving an older target would misreport apply.
	if result.RowsAffected() != 1 {
		return errors.New("promote stable MCP version: runnable family version not found")
	}
	return nil
}

// sameImmutableAppVersion compares compiled code identity and the immutable provider scope.
func sameImmutableAppVersion(existing, requested App) bool {
	// Scalar identity and semantic JSON are checked separately to keep immutable comparison reviewable.
	return sameImmutableAppScalars(existing, requested) &&
		sameJSONDocument(existing.Selections, requested.Selections)
}

// sameImmutableAppScalars pins code bytes, source authority, and provider scope for one version.
func sameImmutableAppScalars(existing, requested App) bool {
	// The compiler output is version identity even when the caller reuses a source label.
	return sameImmutableCodeScalars(existing, requested) && sameImmutableScopeScalars(existing, requested)
}

// sameImmutableCodeScalars binds exact compiler output to source and generation provenance.
func sameImmutableCodeScalars(existing, requested App) bool {
	return existing.SourceHash == requested.SourceHash && existing.BundleDigest == requested.BundleDigest &&
		existing.ConfigKey == requested.ConfigKey && existing.GeneratorVersion == requested.GeneratorVersion
}

// sameImmutableScopeScalars keeps selected provider authority and hosted transport immutable per version.
func sameImmutableScopeScalars(existing, requested App) bool {
	return existing.CapabilityHash == requested.CapabilityHash &&
		existing.ScopeSchemaVersion == requested.ScopeSchemaVersion && existing.HostedMCP == requested.HostedMCP
}

// sameJSONDocument compares semantic JSON so formatting changes cannot mutate immutable app identity.
func sameJSONDocument(existing, requested []byte) bool {
	var existingValue, requestedValue any
	if json.Unmarshal(existing, &existingValue) != nil || json.Unmarshal(requested, &requestedValue) != nil {
		return false
	}
	return reflect.DeepEqual(existingValue, requestedValue)
}

func insertAppCapabilities(ctx context.Context, tx pgx.Tx, appID uuid.UUID, capabilityKeys []string) error {
	if len(capabilityKeys) == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_app_capabilities (app_id, capability_key)
		SELECT $1, capability_key
		FROM unnest($2::text[]) AS capability_key
		ON CONFLICT (app_id, capability_key) DO NOTHING
	`, appID, capabilityKeys)
	if err != nil {
		return fmt.Errorf("publish app: insert capabilities: %w", err)
	}
	return nil
}

func (s *postgresStore) AssessAppCapabilityExpansion(
	ctx context.Context,
	appFamilyID uuid.UUID,
	capabilityKeys []string,
) (bool, int, error) {
	var expands bool
	var tokenCount int
	// Expansion and impact share one database snapshot. Matching strict token
	// scopes here avoids both an N+1 lookup and a race-prone in-memory diff.
	err := s.db.QueryRow(ctx, `
		WITH incoming AS (
			SELECT DISTINCT capability_key
			FROM unnest($2::text[]) AS capability_key
		), runnable_apps AS (
			SELECT app_id
			FROM fused_apps
			WHERE app_family_id = $1 AND status IN ('active', 'deprecated')
		), existing AS (
			SELECT DISTINCT capability.capability_key
			FROM runnable_apps app
			JOIN fused_app_capabilities capability ON capability.app_id = app.app_id
		), missing AS (
			SELECT capability_key FROM incoming
			EXCEPT
			SELECT capability_key FROM existing
		), missing_operations AS (
            -- Imported revision pins still map to the same exact family-token operation grant.
			SELECT regexp_replace(
			         capability_key,
			         '^(service:[^:]+:[^:]+(:mcp:[^:]+)?|unified:[^:]+):operation:',
			         ''
			       ) AS operation_name
			FROM missing
			WHERE capability_key ~ '^(service:[^:]+:[^:]+(:mcp:[^:]+)?|unified:[^:]+):operation:'
		), expansion AS (
			SELECT EXISTS(SELECT 1 FROM runnable_apps)
			   AND EXISTS(SELECT 1 FROM missing) AS expands
		), affected_tokens AS (
			SELECT COUNT(*) AS token_count
			FROM fused_app_tokens token
			WHERE token.app_family_id = $1
			  AND (token.expires_at IS NULL OR token.expires_at > NOW())
			  AND (SELECT expands FROM expansion)
			  AND (
			    token.allow_all
			    OR EXISTS (
			      SELECT 1
			      FROM missing_operations operation
			      WHERE operation.operation_name = ANY(token.allowed_operations)
			    )
			  )
		)
		SELECT expansion.expands, affected_tokens.token_count
		FROM expansion CROSS JOIN affected_tokens
	`, appFamilyID, capabilityKeys).Scan(&expands, &tokenCount)
	if err != nil {
		return false, 0, fmt.Errorf("assess app capability expansion: %w", err)
	}
	return expands, tokenCount, nil
}

// lockAppFamily serializes publication against family deletion and rejects archived identities.
func lockAppFamily(ctx context.Context, tx pgx.Tx, app App) error {
	var found uuid.UUID
	var kind AppKind
	err := tx.QueryRow(ctx, `
		SELECT app_family_id, kind FROM fused_app_families
		WHERE app_family_id = $1 AND account_id = $2 AND archived_at IS NULL
		FOR UPDATE
	`, app.AppFamilyID, app.AccountID).Scan(&found, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppFamilyNotFound
	}
	if err != nil {
		return fmt.Errorf("publish app: lock family: %w", err)
	}
	if kind != app.ExpectedFamilyKind {
		return ErrAppFamilyKindMismatch
	}
	return nil
}

func loadVersionForPublish(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, version string) (*App, error) {
	app, err := scanApp(tx.QueryRow(ctx, appSelect+`
		WHERE a.app_family_id = $1 AND a.version = $2
		FOR UPDATE`, familyID, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("publish app: load version: %w", err)
	}
	return app, nil
}

func rejectTombstonedVersion(ctx context.Context, tx pgx.Tx, familyID uuid.UUID, version string) error {
	var exists bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM fused_app_tombstones
			WHERE app_family_id = $1 AND version = $2
		)
	`, familyID, version).Scan(&exists)
	if err != nil {
		return fmt.Errorf("publish app: check tombstone: %w", err)
	}
	if exists {
		return ErrAppTombstoneExists
	}
	return nil
}

// insertApp writes the immutable code and provider scope in one publication transaction.
func insertApp(ctx context.Context, tx pgx.Tx, app App) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key,
			 source_hash, bundle_digest, capability_hash, scope_schema_version, selections,
			 generator_version, sdk_generation_job_id, sdk_generation_status, hosted_mcp,
			 status, created_by, activated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), $8, $9, $10,
		        NULLIF($11, ''), NULLIF($12, ''), NULLIF($13, ''), $14, $15,
		        NULLIF($16, '00000000-0000-0000-0000-000000000000'::uuid),
		        CASE WHEN $15 = 'active' THEN NOW() ELSE NULL END)
	`, app.AppID, app.AppFamilyID, app.AccountID, app.Version, app.ConfigKey,
		app.SourceHash, app.BundleDigest, app.CapabilityHash, app.ScopeSchemaVersion, app.Selections,
		app.GeneratorVersion, app.SDKGenerationJobID, app.SDKGenerationStatus,
		app.HostedMCP, app.Status, app.CreatedBy)
	if err != nil {
		return fmt.Errorf("publish app: insert: %w", err)
	}
	return nil
}

const appSelect = `
SELECT a.app_id, a.app_family_id, a.account_id, a.version, a.config_key,
       a.source_hash, COALESCE(a.bundle_digest, ''), a.capability_hash, a.scope_schema_version, a.selections,
	       COALESCE(a.generator_version, ''),
	       COALESCE(a.sdk_generation_job_id, ''), COALESCE(a.sdk_generation_status, ''),
	       a.hosted_mcp, a.status,
       COALESCE(a.deprecation_message, ''), a.planned_deactivation_at,
       COALESCE(a.created_by, '00000000-0000-0000-0000-000000000000'::uuid),
	   a.created_at, a.activated_at
FROM fused_apps a`

func (s *postgresStore) GetApp(ctx context.Context, appID uuid.UUID) (*App, error) {
	row := s.db.QueryRow(ctx, appSelect+` WHERE a.app_id = $1`, appID)
	app, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	return app, err
}

func (s *postgresStore) GetAppByFamilyAndVersion(ctx context.Context, appFamilyID uuid.UUID, version string) (*App, error) {
	row := s.db.QueryRow(ctx, appSelect+`
		WHERE a.app_family_id = $1 AND a.version = $2`, appFamilyID, version)
	app, err := scanApp(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	return app, err
}

func (s *postgresStore) ListApps(ctx context.Context, appFamilyID uuid.UUID) ([]App, error) {
	rows, err := s.db.Query(ctx, appSelect+`
		WHERE a.app_family_id = $1
		ORDER BY a.created_at DESC
		LIMIT $2`, appFamilyID, appFamilyCollectionLimit)
	if err != nil {
		return nil, fmt.Errorf("list apps: %w", err)
	}
	defer rows.Close()

	var apps []App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, fmt.Errorf("scan app: %w", err)
		}
		apps = append(apps, *a)
	}
	return apps, rows.Err()
}

// ResolveMCPRoute resolves one UUID as either an exact MCP version or its
// promoted family target without loading candidates into memory.
func (s *postgresStore) ResolveMCPRoute(ctx context.Context, routeID uuid.UUID) (*MCPRouteTarget, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.mcp_route.resolve")
	defer span.End()
	var target MCPRouteTarget
	err := s.db.QueryRow(ctx, `
		WITH candidates AS (
			SELECT app.app_family_id, app.app_id, false AS stable, 0 AS preference
			FROM fused_apps app
			JOIN fused_app_families family
			  ON family.app_family_id = app.app_family_id
			 AND family.account_id = app.account_id
			WHERE app.app_id = $1
			  AND (family.kind = 'mcp' OR (family.kind IN ('sdk', 'unified_app') AND app.hosted_mcp))
			  AND (family.kind <> 'unified_app' OR family.unified_active_app_id = app.app_id)
			  AND app.status IN ('active', 'deprecated')
			UNION ALL
			SELECT family.app_family_id, app.app_id, true AS stable, 1 AS preference
			FROM fused_app_families family
			JOIN fused_apps app
			  ON app.app_id = family.mcp_stable_app_id
			 AND app.app_family_id = family.app_family_id
			 AND app.account_id = family.account_id
			WHERE family.app_family_id = $1
			  AND (family.kind = 'mcp' OR (family.kind IN ('sdk', 'unified_app') AND app.hosted_mcp))
			  AND (family.kind <> 'unified_app' OR family.unified_active_app_id = app.app_id)
			  AND app.status IN ('active', 'deprecated')
		)
		SELECT app_family_id, app_id, stable
		FROM candidates
		ORDER BY preference
		LIMIT 1
	`, routeID).Scan(&target.AppFamilyID, &target.AppID, &target.Stable)
	// Unknown, deactivated, and unpromoted identities share one closed result so
	// route discovery cannot reveal lifecycle state to an unauthenticated peer.
	if errors.Is(err, pgx.ErrNoRows) {
		span.SetAttributes(attribute.String("outcome", "not_found"))
		return nil, ErrAppNotFound
	}
	// Unexpected persistence failures remain internal and cannot be collapsed
	// into the public not-found lifecycle result.
	if err != nil {
		return nil, fmt.Errorf("resolve MCP route: %w", err)
	}
	span.SetAttributes(
		attribute.String("outcome", "resolved"),
		attribute.Bool("mcp.route.stable", target.Stable),
		attribute.String("app.family_id", target.AppFamilyID.String()),
		attribute.String("app.id", target.AppID.String()),
	)
	return &target, nil
}

func (s *postgresStore) ListSDKPackageLeaseRenewals(ctx context.Context, after uuid.UUID, limit int) ([]models.SDKPackageLeaseRenewal, error) {
	if limit <= 0 || limit > models.SDKPackageLeaseBatchLimit {
		limit = models.SDKPackageLeaseBatchLimit
	}
	rows, err := s.db.Query(ctx, `
		SELECT app.app_id, app.app_family_id
		FROM fused_apps app
		JOIN fused_app_families family
		  ON family.app_family_id = app.app_family_id
		 AND family.account_id = app.account_id
		WHERE family.kind = 'sdk'
		  AND app.status IN ('active', 'deprecated')
		  AND ($1 = '00000000-0000-0000-0000-000000000000'::uuid OR app.app_id > $1)
		ORDER BY app.app_id
		LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list SDK package lease renewals: %w", err)
	}
	defer rows.Close()

	renewals := make([]models.SDKPackageLeaseRenewal, 0, limit)
	for rows.Next() {
		var renewal models.SDKPackageLeaseRenewal
		if err := rows.Scan(&renewal.AppID, &renewal.AppFamilyID); err != nil {
			return nil, fmt.Errorf("scan SDK package lease renewal: %w", err)
		}
		renewals = append(renewals, renewal)
	}
	return renewals, rows.Err()
}

// ErrSDKPackageNotGenerated distinguishes an authorized direct API from a missing generated package.
var ErrSDKPackageNotGenerated = errors.New("app has no generated SDK package")

// GetSDKPackageBuildRequest admits only generated delivery before reconstructing exact pinned package inputs.
func (s *postgresStore) GetSDKPackageBuildRequest(ctx context.Context, accountID, appID uuid.UUID) (*models.SDKGenerationRequest, error) {
	var request models.SDKGenerationRequest
	var deliveryMode AppDeliveryMode
	var selections, bindings, attached []byte
	var planID uuid.UUID
	err := s.db.QueryRow(ctx, `
		SELECT family.display_name, app.version, app.app_family_id, app.app_id,
		       app.source_hash, COALESCE(app.generator_version, ''),
		       family.target_language, app.selections,
		       COALESCE(plan.resolved_payload->>'description', ''),
		       COALESCE((plan.resolved_payload->>'include_mcp')::boolean, false),
		       COALESCE((plan.resolved_payload->>'skip_sandbox')::boolean, false),
		       COALESCE(plan.resolved_payload->>'default_engine_url', ''),
		       COALESCE(plan.resolved_payload->'contract_bindings', '[]'::jsonb),
		       plan.id, COALESCE(family.delivery_mode, ''), COALESCE(plan.resolved_payload->'unified_apps', '[]'::jsonb)
		FROM fused_apps app
		JOIN fused_app_families family
		  ON family.app_family_id = app.app_family_id
		 AND family.account_id = app.account_id
		JOIN LATERAL (
			SELECT applied.id, applied.resolved_payload
			FROM fused_config_plans applied
			WHERE applied.config_key = app.config_key
			  AND applied.source_hash = app.source_hash
			  AND applied.status = 'applied'
			  AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean, false)
			ORDER BY applied.applied_at DESC, applied.created_at DESC
			LIMIT 1
		) plan ON true
		WHERE app.account_id = $1 AND app.app_id = $2
		  AND family.kind = 'sdk'
		  AND app.status IN ('active', 'deprecated')
	`, accountID, appID).Scan(
		&request.Name, &request.Version, &request.AppFamilyID, &request.AppID,
		&request.SourceHash, &request.GeneratorVersion, &request.TargetLanguage,
		&selections, &request.Description, &request.IncludeMCP, &request.SkipSandbox,
		&request.DefaultEngineURL, &bindings, &planID, &deliveryMode, &attached,
	)
	// Absence and cross-account identity remain indistinguishable to callers.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	// A failed lookup cannot authorize package reconstruction.
	if err != nil {
		return nil, fmt.Errorf("get SDK package build request: %w", err)
	}
	// Direct APIs must not reach the Registry cache or its cache-miss generation path.
	if deliveryMode != AppDeliveryModeSDK {
		return nil, ErrSDKPackageNotGenerated
	}
	// Pinned inputs must decode successfully; never substitute current catalogue state.
	if err := json.Unmarshal(selections, &request.Selections); err != nil {
		return nil, fmt.Errorf("decode SDK package selections: %w", err)
	}
	// Bindings retain the applied version's generation authority.
	if err := json.Unmarshal(bindings, &request.ContractBindings); err != nil {
		return nil, fmt.Errorf("decode SDK package contract bindings: %w", err)
	}
	// Package recovery must retain the same public hosted contracts as its original build.
	if err := json.Unmarshal(attached, &request.UnifiedApps); err != nil {
		return nil, err
	}
	request.IdempotencyKey = planID.String()
	request.TargetType = AppKindSDK.String()
	return &request, nil
}

// scanApp maps the stable query column order into one immutable app publication value.
func scanApp(row pgx.Row) (*App, error) {
	var a App
	var depMsg string
	err := row.Scan(&a.AppID, &a.AppFamilyID, &a.AccountID, &a.Version, &a.ConfigKey,
		&a.SourceHash, &a.BundleDigest, &a.CapabilityHash, &a.ScopeSchemaVersion, &a.Selections,
		&a.GeneratorVersion, &a.SDKGenerationJobID, &a.SDKGenerationStatus,
		&a.HostedMCP, &a.Status, &depMsg, &a.PlannedDeactivationAt,
		&a.CreatedBy, &a.CreatedAt, &a.ActivatedAt)
	if err != nil {
		return nil, err
	}
	if depMsg != "" {
		a.DeprecationMessage = depMsg
	}
	return &a, nil
}

// --- App lifecycle: deprecation, undeprecation, deactivation ---

func (s *postgresStore) DeprecateApp(ctx context.Context, appID uuid.UUID, message string, plannedDeactivationAt *time.Time) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.deprecate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	tag, err := s.db.Exec(ctx, `
		UPDATE fused_apps
		SET status = 'deprecated',
		    deprecation_message = $2,
		    deprecated_at = NOW(),
		    planned_deactivation_at = $3
		WHERE app_id = $1 AND status IN ('active', 'deprecated')
	`, appID, message, plannedDeactivationAt)
	if err != nil {
		return fmt.Errorf("deprecate app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAppNotFound
	}
	return nil
}

func (s *postgresStore) UndeprecateApp(ctx context.Context, appID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.undeprecate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	tag, err := s.db.Exec(ctx, `
		UPDATE fused_apps
		SET status = 'active',
		    deprecation_message = NULL,
		    deprecated_at = NULL,
		    planned_deactivation_at = NULL
		WHERE app_id = $1 AND status = 'deprecated'
	`, appID)
	if err != nil {
		return fmt.Errorf("undeprecate app: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAppNotFound
	}
	return nil
}

func (s *postgresStore) DeactivateAppVersion(ctx context.Context, appID, deactivatedBy uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.deactivate")
	defer span.End()
	span.SetAttributes(attribute.String("app.id", appID.String()))

	// Tombstone, config-state removal, and scope deletion share one statement so
	// a failed destructive action cannot leave an executable app half-deactivated.
	var deletedID uuid.UUID
	err := s.db.QueryRow(ctx, `
		WITH selected AS (
			SELECT app_id, app_family_id, account_id, version, source_hash, config_key
			FROM fused_apps
			WHERE app_id = $1 AND status IN ('active', 'deprecated')
			FOR UPDATE
		), tombstoned AS (
			INSERT INTO fused_app_tombstones
				(app_id, app_family_id, account_id, version, source_hash, deactivated_by)
			SELECT app_id, app_family_id, account_id, version, source_hash,
			       NULLIF($2, '00000000-0000-0000-0000-000000000000'::uuid)
			FROM selected
			ON CONFLICT (app_family_id, version) DO NOTHING
			RETURNING app_id
		), removed_config AS (
			DELETE FROM fused_config_states state
			USING selected, tombstoned
			WHERE state.config_key = selected.config_key
			  AND tombstoned.app_id = selected.app_id
		), ended_sessions AS (
			UPDATE fused_mcp_sessions session
			SET ended_at = COALESCE(session.ended_at, NOW())
			FROM selected, tombstoned
			WHERE session.app_id = selected.app_id
			  AND tombstoned.app_id = selected.app_id
		), removed_idempotency AS (
			DELETE FROM fused_engine_idempotency_keys execution
			USING selected, tombstoned
			WHERE execution.app_id = selected.app_id
			  AND tombstoned.app_id = selected.app_id
		), removed_app AS (
			DELETE FROM fused_apps app
			USING tombstoned
			WHERE app.app_id = tombstoned.app_id
			RETURNING app.app_id
		)
		SELECT app_id FROM removed_app
	`, appID, deactivatedBy).Scan(&deletedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppNotFound
	}
	if err != nil {
		return fmt.Errorf("deactivate app: %w", err)
	}
	return nil
}

// --- Family-token authorization ---

// AuthorizeApp admits an exact runnable version only through a live family and active family token.
func (s *postgresStore) AuthorizeApp(ctx context.Context, appID uuid.UUID, tokenHash string) (*AuthProjection, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.authorize")
	defer span.End()

	var proj AuthProjection
	err := s.db.QueryRow(ctx, `
		WITH matched AS (
			SELECT a.account_id, f.app_family_id, a.app_id, a.version, f.kind, a.hosted_mcp, a.status,
			       t.id AS token_id, t.allow_all, t.allowed_operations, t.expires_at,
			       t.binding_mode
			FROM fused_apps a
			JOIN fused_app_families f
			  ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
			JOIN fused_app_tokens t ON t.app_family_id = f.app_family_id
			WHERE a.app_id = $1 AND t.token_hash = $2
			  AND f.archived_at IS NULL
			  AND a.status IN ('active', 'deprecated')
			  AND (t.expires_at IS NULL OR t.expires_at > NOW())
		), touched AS (
			UPDATE fused_app_tokens token
			SET last_used_at = NOW()
			FROM matched
			WHERE token.id = matched.token_id
			RETURNING token.id
		)
		SELECT account_id, app_family_id, app_id, token_id, version, kind, hosted_mcp, status,
		       allow_all, allowed_operations, expires_at, binding_mode
		FROM matched
		WHERE EXISTS (SELECT 1 FROM touched)
	`, appID, tokenHash).Scan(
		&proj.AccountID, &proj.AppFamilyID, &proj.AppID, &proj.TokenID, &proj.Version, &proj.Kind, &proj.HostedMCP, &proj.AppStatus,
		&proj.TokenPolicy.AllowAll, &proj.TokenPolicy.AllowedOperations, &proj.TokenPolicy.ExpiresAt,
		&proj.BindingMode,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		span.SetAttributes(attribute.String("outcome", "denied"))
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("authorize app: %w", err)
	}

	span.SetAttributes(
		attribute.String("app.family_id", proj.AppFamilyID.String()),
		attribute.String("app.id", proj.AppID.String()),
		attribute.String("app.status", proj.AppStatus.String()),
		attribute.String("outcome", "allowed"),
	)
	return &proj, nil
}

// --- Family buckets ---

// SetAppFamilyServiceBucket upserts a per-service bucket override for one
// family. The conflict target is the UNIQUE(app_family_id, service_id)
// constraint, distinct from the default row's partial unique index because
// service_id is never NULL here.
func (s *postgresStore) SetAppFamilyServiceBucket(ctx context.Context, appFamilyID, serviceID, bucketID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, service_id, bucket_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (app_family_id, service_id) DO UPDATE SET
			bucket_id = EXCLUDED.bucket_id,
			updated_at = NOW()
	`, appFamilyID, serviceID, bucketID)
	if err != nil {
		return fmt.Errorf("set app family service bucket: %w", err)
	}
	return nil
}

// ResolveAppFamilyServiceBucket returns the bucket a specific service
// resolves through: its own override if one exists, otherwise the family
// default. ORDER BY service_id NULLS LAST plus LIMIT 1 lets a single query
// prefer the override row over the default row without a second round trip
// (this is the hot path used by runtime credential resolution).
func (s *postgresStore) ResolveAppFamilyServiceBucket(ctx context.Context, appFamilyID, serviceID uuid.UUID) (*AppFamilyBucket, error) {
	var fb AppFamilyBucket
	err := s.db.QueryRow(ctx, `
		SELECT app_family_id, bucket_id, created_at, updated_at
		FROM fused_app_family_buckets
		WHERE app_family_id = $1 AND (service_id = $2 OR service_id IS NULL)
		ORDER BY service_id NULLS LAST
		LIMIT 1
	`, appFamilyID, serviceID).Scan(&fb.AppFamilyID, &fb.BucketID, &fb.CreatedAt, &fb.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolve app family service bucket: %w", err)
	}
	return &fb, nil
}

// ListAppFamilyServiceBuckets batches every per-service override for one
// family into a single query, keyed by service ID, so plan/apply diffing
// (deciding which overrides to add/update/remove) never issues one query
// per service.
func (s *postgresStore) ListAppFamilyServiceBuckets(ctx context.Context, appFamilyID uuid.UUID) (map[uuid.UUID]AppFamilyBucket, error) {
	rows, err := s.db.Query(ctx, `
		SELECT app_family_id, service_id, bucket_id, created_at, updated_at
		FROM fused_app_family_buckets
		WHERE app_family_id = $1 AND service_id IS NOT NULL
	`, appFamilyID)
	if err != nil {
		return nil, fmt.Errorf("list app family service buckets: %w", err)
	}
	defer rows.Close()

	out := make(map[uuid.UUID]AppFamilyBucket)
	for rows.Next() {
		var fb AppFamilyBucket
		var serviceID uuid.UUID
		if err := rows.Scan(&fb.AppFamilyID, &serviceID, &fb.BucketID, &fb.CreatedAt, &fb.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan app family service bucket: %w", err)
		}
		out[serviceID] = fb
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list app family service buckets: %w", err)
	}
	return out, nil
}

func (s *postgresStore) AppTombstoneExists(ctx context.Context, appFamilyID uuid.UUID, version string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM fused_app_tombstones
			WHERE app_family_id = $1 AND version = $2
		)
	`, appFamilyID, version).Scan(&exists)
	return exists, err
}
