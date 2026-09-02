package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
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
			 target_language, owner_subject_id, owner_team_id)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''),
		        NULLIF($7, '00000000-0000-0000-0000-000000000000'::uuid),
		        NULLIF($8, '00000000-0000-0000-0000-000000000000'::uuid))
		ON CONFLICT (account_id, kind, canonical_name) DO UPDATE
		SET updated_at = family.updated_at
		RETURNING app_family_id, account_id, kind, canonical_name, display_name,
		          COALESCE(target_language, ''),
		          COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		          COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
		          created_at, updated_at, (xmax = 0)
	`, family.AppFamilyID, family.AccountID, family.Kind, family.CanonicalName,
		family.DisplayName, family.TargetLanguage, family.OwnerSubjectID, family.OwnerTeamID)
	var result AppFamily
	var created bool
	err := row.Scan(&result.AppFamilyID, &result.AccountID, &result.Kind,
		&result.CanonicalName, &result.DisplayName, &result.TargetLanguage,
		&result.OwnerSubjectID, &result.OwnerTeamID, &result.CreatedAt,
		&result.UpdatedAt, &created)
	if err != nil {
		return nil, false, fmt.Errorf("create or get app family: %w", err)
	}
	return &result, created, nil
}

const appFamilySelect = `
SELECT app_family_id, account_id, kind, canonical_name, display_name,
       COALESCE(target_language, ''),
       COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
       COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
       created_at, updated_at
FROM fused_app_families`

func (s *postgresStore) GetAppFamily(ctx context.Context, appFamilyID uuid.UUID) (*AppFamily, error) {
	row := s.db.QueryRow(ctx, appFamilySelect+` WHERE app_family_id = $1`, appFamilyID)
	f, err := scanAppFamily(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppFamilyNotFound
	}
	return f, err
}

func (s *postgresStore) GetAppFamilyByIdentity(ctx context.Context, accountID uuid.UUID, kind, canonicalName string) (*AppFamily, error) {
	row := s.db.QueryRow(ctx, appFamilySelect+`
		WHERE account_id = $1 AND kind = $2 AND canonical_name = $3`,
		accountID, kind, canonicalName)
	f, err := scanAppFamily(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppFamilyNotFound
	}
	return f, err
}

func (s *postgresStore) ListAppFamilies(ctx context.Context, accountID uuid.UUID, kind string, limit, offset int) ([]AppFamily, int, error) {
	rows, err := s.db.Query(ctx, `
		SELECT app_family_id, account_id, kind, canonical_name, display_name,
		       COALESCE(target_language, ''),
		       COALESCE(owner_subject_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(owner_team_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       created_at, updated_at, COUNT(*) OVER()
		FROM fused_app_families
		WHERE account_id = $1 AND ($2 = '' OR kind = $2)
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
			&f.DisplayName, &f.TargetLanguage, &f.OwnerSubjectID, &f.OwnerTeamID,
			&f.CreatedAt, &f.UpdatedAt, &total); err != nil {
			return nil, 0, fmt.Errorf("scan app family: %w", err)
		}
		families = append(families, f)
	}
	return families, total, rows.Err()
}

func scanAppFamily(row pgx.Row) (*AppFamily, error) {
	var f AppFamily
	err := row.Scan(&f.AppFamilyID, &f.AccountID, &f.Kind, &f.CanonicalName,
		&f.DisplayName, &f.TargetLanguage, &f.OwnerSubjectID, &f.OwnerTeamID,
		&f.CreatedAt, &f.UpdatedAt)
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
			SELECT family.canonical_name,
			       EXISTS (
				 SELECT 1
				 FROM fused_apps app
				 WHERE app.app_family_id = family.app_family_id
				   AND app.account_id = family.account_id
				   AND app.status IN ('active', 'deprecated')
				   AND (
				     $2 NOT IN ('api', 'sdk')
				     OR ($2 = 'api' AND app.sdk_generation_status = 'skipped')
				     OR ($2 = 'sdk' AND app.sdk_generation_status IS DISTINCT FROM 'skipped')
				   )
			       ) AS invokable
			FROM fused_app_families family
			WHERE family.account_id = $1
			  AND (
			    $2 = ''
			    OR ($2 = 'api' AND family.kind = 'sdk')
			    OR ($2 <> 'api' AND family.kind = $2)
			  )
		)
		SELECT COUNT(*) FILTER (WHERE invokable),
		       COALESCE(BOOL_OR(canonical_name = $3 AND invokable), FALSE)
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
	app = withUnifiedDefaults(app)
	if err := lockAppFamily(ctx, tx, app); err != nil {
		return nil, false, err
	}
	existing, err := loadVersionForPublish(ctx, tx, app.AppFamilyID, app.Version)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		// Immutable equality permits reapplying an older MCP declaration; that
		// explicit apply must still move the stable family route back to it.
		if !sameImmutableAppVersion(*existing, app) {
			return nil, false, ErrAppVersionImmutable
		}
		if err := promoteStableMCPVersionTx(ctx, tx, app); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
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

// promoteStableMCPVersionTx advances the MCP family alias in the same transaction
// that proves the immutable version exists and is runnable.
func promoteStableMCPVersionTx(ctx context.Context, tx pgx.Tx, app App) error {
	// SDK execution remains version-pinned, so only MCP publication owns a
	// mutable transport target.
	if app.ExpectedFamilyKind != AppKindMCP || !app.Status.Runnable() {
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
		  AND family.kind = 'mcp'
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

// withUnifiedDefaults maps legacy callers to the canonical empty Unified definition set before publication.
func withUnifiedDefaults(app App) App {
	// Existing SDK/MCP callers carry no Unified fields. Giving the empty set a
	// canonical identity here keeps every publication path on the same immutable
	// contract without making adapters duplicate defaults.
	if app.UnifiedDefinitionSchemaVersion == 0 {
		app.UnifiedDefinitionSchemaVersion = UnifiedDefinitionSchemaVersion
	}
	if len(app.UnifiedDefinitions) == 0 {
		app.UnifiedDefinitions = []byte("[]")
	}
	if app.UnifiedDefinitionHash == "" {
		app.UnifiedDefinitionHash = EmptyUnifiedSetHash
	}
	if app.UnifiedCodegenDescriptorHash == "" {
		app.UnifiedCodegenDescriptorHash = EmptyUnifiedSetHash
	}
	return app
}

// sameImmutableAppVersion compares private definitions and hashes alongside the existing immutable app scope.
func sameImmutableAppVersion(existing, requested App) bool {
	existing = withUnifiedDefaults(existing)
	requested = withUnifiedDefaults(requested)
	if existing.SourceHash != requested.SourceHash || existing.ConfigKey != requested.ConfigKey ||
		existing.CapabilityHash != requested.CapabilityHash || existing.ScopeSchemaVersion != requested.ScopeSchemaVersion ||
		existing.GeneratorVersion != requested.GeneratorVersion ||
		existing.UnifiedDefinitionSchemaVersion != requested.UnifiedDefinitionSchemaVersion ||
		existing.UnifiedDefinitionHash != requested.UnifiedDefinitionHash ||
		existing.UnifiedCodegenDescriptorHash != requested.UnifiedCodegenDescriptorHash {
		return false
	}
	if !sameJSONDocument(existing.Selections, requested.Selections) {
		return false
	}
	return sameJSONDocument(existing.UnifiedDefinitions, requested.UnifiedDefinitions)
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
			SELECT regexp_replace(
			         capability_key,
			         '^service:[^:]+:[^:]+:operation:',
			         ''
			       ) AS operation_name
			FROM missing
			WHERE capability_key ~ '^service:[^:]+:[^:]+:operation:'
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

func lockAppFamily(ctx context.Context, tx pgx.Tx, app App) error {
	var found uuid.UUID
	var kind AppKind
	err := tx.QueryRow(ctx, `
		SELECT app_family_id, kind FROM fused_app_families
		WHERE app_family_id = $1 AND account_id = $2
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

// insertApp writes one immutable app version together with its private Unified definitions and hashes.
func insertApp(ctx context.Context, tx pgx.Tx, app App) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_apps
			(app_id, app_family_id, account_id, version, config_key,
			 source_hash, capability_hash, scope_schema_version, selections,
			 unified_definition_schema_version, unified_definitions,
			 unified_definition_hash, unified_codegen_descriptor_hash,
			 generator_version, sdk_generation_job_id, sdk_generation_status,
			 status, created_by, activated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		        NULLIF($14, ''), NULLIF($15, ''), NULLIF($16, ''), $17,
		        NULLIF($18, '00000000-0000-0000-0000-000000000000'::uuid),
		        CASE WHEN $17 = 'active' THEN NOW() ELSE NULL END)
	`, app.AppID, app.AppFamilyID, app.AccountID, app.Version, app.ConfigKey,
		app.SourceHash, app.CapabilityHash, app.ScopeSchemaVersion, app.Selections,
		app.UnifiedDefinitionSchemaVersion, app.UnifiedDefinitions,
		app.UnifiedDefinitionHash, app.UnifiedCodegenDescriptorHash,
		app.GeneratorVersion, app.SDKGenerationJobID, app.SDKGenerationStatus,
		app.Status, app.CreatedBy)
	if err != nil {
		return fmt.Errorf("publish app: insert: %w", err)
	}
	return nil
}

const appSelect = `
SELECT a.app_id, a.app_family_id, a.account_id, a.version, a.config_key,
       a.source_hash, a.capability_hash, a.scope_schema_version, a.selections,
       a.unified_definition_schema_version, a.unified_definitions,
	       a.unified_definition_hash, a.unified_codegen_descriptor_hash,
	       COALESCE(a.generator_version, ''),
	       COALESCE(a.sdk_generation_job_id, ''), COALESCE(a.sdk_generation_status, ''),
	       a.status,
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
			  AND family.kind = 'mcp'
			  AND app.status IN ('active', 'deprecated')
			UNION ALL
			SELECT family.app_family_id, app.app_id, true AS stable, 1 AS preference
			FROM fused_app_families family
			JOIN fused_apps app
			  ON app.app_id = family.mcp_stable_app_id
			 AND app.app_family_id = family.app_family_id
			 AND app.account_id = family.account_id
			WHERE family.app_family_id = $1
			  AND family.kind = 'mcp'
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

// GetSDKPackageBuildRequest returns exact sdk package build request through one app-scoped query or cache lookup.
func (s *postgresStore) GetSDKPackageBuildRequest(ctx context.Context, accountID, appID uuid.UUID) (*models.SDKGenerationRequest, error) {
	var request models.SDKGenerationRequest
	var selections, bindings, unifiedOperations []byte
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
		       COALESCE(plan.resolved_payload->'unified_operations', 'null'::jsonb),
		       plan.id
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
		&request.DefaultEngineURL, &bindings, &unifiedOperations, &planID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get SDK package build request: %w", err)
	}
	if err := json.Unmarshal(selections, &request.Selections); err != nil {
		return nil, fmt.Errorf("decode SDK package selections: %w", err)
	}
	if err := json.Unmarshal(bindings, &request.ContractBindings); err != nil {
		return nil, fmt.Errorf("decode SDK package contract bindings: %w", err)
	}
	if string(unifiedOperations) != "null" {
		if err := json.Unmarshal(unifiedOperations, &request.UnifiedOperations); err != nil {
			return nil, fmt.Errorf("decode SDK package unified operations: %w", err)
		}
	}
	request.IdempotencyKey = planID.String()
	request.TargetType = AppKindSDK.String()
	return &request, nil
}

// GetMCPUnifiedOperationDescriptors returns only complete logical operations
// whose physical graph is discoverable under the session token. The complete
// descriptor is still read for immutable-hash verification, while PostgreSQL
// owns policy filtering so runtime code cannot accidentally broaden it.
func (s *postgresStore) GetMCPUnifiedOperationDescriptors(ctx context.Context, appID uuid.UUID, unrestricted bool, allowedOperations []string) (*models.SDKUnifiedOperationDescriptors, error) {
	var expectedHash string
	var complete, visible []byte
	err := s.db.QueryRow(ctx, `
		WITH descriptor AS (
			SELECT app.unified_codegen_descriptor_hash AS expected_hash, COALESCE(plan.resolved_payload->'unified_operations', 'null'::jsonb) AS complete
			FROM fused_apps app
			JOIN fused_app_families family ON family.app_family_id = app.app_family_id AND family.account_id = app.account_id
			JOIN LATERAL (
				SELECT applied.resolved_payload
				FROM fused_config_plans applied
				WHERE applied.config_key = app.config_key AND applied.source_hash = app.source_hash
				  AND applied.config_type = 'mcp' AND applied.status = 'applied'
				  AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean, false)
				ORDER BY applied.applied_at DESC, applied.created_at DESC
				LIMIT 1
			) plan ON true
			WHERE app.app_id = $1 AND family.kind = 'mcp' AND app.status IN ('active', 'deprecated')
		), projected AS (
			SELECT expected_hash, complete,
			       CASE WHEN complete = 'null'::jsonb THEN complete
			       ELSE jsonb_build_object(
			         'schema_version', complete->'schema_version',
			         'operations', COALESCE((
			           SELECT jsonb_agg(candidate.operation ORDER BY candidate.operation->>'name')
			           FROM jsonb_array_elements(complete->'operations') candidate(operation)
			           WHERE $2::boolean OR NOT EXISTS (
			             SELECT 1
			             FROM jsonb_array_elements(candidate.operation->'targets') target(value)
			             WHERE NOT (target.value->>'operation_id' = ANY($3::text[]) AND (
			                 target.value->'rollback' IS NULL
			                 OR target.value->'rollback' = 'null'::jsonb
			                 OR target.value->'rollback'->>'operation_id' = ANY($3::text[])
			               ))
			           )
			         ), '[]'::jsonb)
			       ) END AS visible
			FROM descriptor
		)
		SELECT expected_hash, complete, visible FROM projected
	`, appID, unrestricted, allowedOperations).Scan(&expectedHash, &complete, &visible)
	// A missing runnable MCP version or exact applied plan cannot provide an
	// authoritative catalogue and must not fall back to mutable state.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	// Database errors remain distinct from an absent descriptor so callers can
	// fail the session rather than silently presenting a partial catalogue.
	if err != nil {
		return nil, fmt.Errorf("get MCP Unified descriptor: %w", err)
	}
	return decodeMCPUnifiedDescriptorProjection(complete, visible, expectedHash)
}

// decodeMCPUnifiedDescriptorProjection verifies the immutable complete value
// before admitting the independently SQL-filtered public projection.
func decodeMCPUnifiedDescriptorProjection(complete, visible []byte, expectedHash string) (*models.SDKUnifiedOperationDescriptors, error) {
	// The canonical empty graph is represented by no public descriptor, not by
	// a second synthetic descriptor shape.
	if string(complete) == "null" {
		if expectedHash != EmptyUnifiedSetHash {
			return nil, errors.New("MCP Unified descriptor hash is inconsistent")
		}
		return nil, nil
	}
	digest, err := canonicaljson.HexSHA256(complete)
	// App identity pins the complete descriptor, so a mismatch invalidates the
	// entire discovery surface even if the filtered subset decoded cleanly.
	if err != nil || "sha256:"+digest != expectedHash {
		return nil, errors.New("MCP Unified descriptor hash is invalid")
	}
	var descriptors models.SDKUnifiedOperationDescriptors
	// The filtered bytes retain the shared public descriptor contract; they do
	// not become a separate MCP-specific schema.
	if err := json.Unmarshal(visible, &descriptors); err != nil {
		return nil, fmt.Errorf("decode MCP Unified descriptor: %w", err)
	}
	// Runtime discovery accepts only the compiler's current public descriptor
	// schema so future semantics cannot be guessed by an older Engine.
	if descriptors.SchemaVersion != models.SDKUnifiedDescriptorSchemaVersion {
		return nil, errors.New("MCP Unified descriptor schema is unsupported")
	}
	// No fully authorized logical operation means there is no Unified catalogue
	// section for this token, while physical discovery remains unaffected.
	if len(descriptors.Operations) == 0 {
		return nil, nil
	}
	return &descriptors, nil
}

// scanApp maps the stable query column order into one immutable app publication value.
func scanApp(row pgx.Row) (*App, error) {
	var a App
	var depMsg string
	err := row.Scan(&a.AppID, &a.AppFamilyID, &a.AccountID, &a.Version, &a.ConfigKey,
		&a.SourceHash, &a.CapabilityHash, &a.ScopeSchemaVersion, &a.Selections,
		&a.UnifiedDefinitionSchemaVersion, &a.UnifiedDefinitions,
		&a.UnifiedDefinitionHash, &a.UnifiedCodegenDescriptorHash,
		&a.GeneratorVersion, &a.SDKGenerationJobID, &a.SDKGenerationStatus,
		&a.Status, &depMsg, &a.PlannedDeactivationAt,
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

func (s *postgresStore) AuthorizeApp(ctx context.Context, appID uuid.UUID, tokenHash string) (*AuthProjection, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.store.app.authorize")
	defer span.End()

	var proj AuthProjection
	err := s.db.QueryRow(ctx, `
		WITH matched AS (
			SELECT a.account_id, f.app_family_id, a.app_id, a.version, f.kind, a.status,
			       t.id AS token_id, t.allow_all, t.allowed_operations, t.expires_at,
			       t.binding_mode
			FROM fused_apps a
			JOIN fused_app_families f
			  ON f.app_family_id = a.app_family_id AND f.account_id = a.account_id
			JOIN fused_app_tokens t ON t.app_family_id = f.app_family_id
			WHERE a.app_id = $1 AND t.token_hash = $2
			  AND a.status IN ('active', 'deprecated')
			  AND (t.expires_at IS NULL OR t.expires_at > NOW())
		), touched AS (
			UPDATE fused_app_tokens token
			SET last_used_at = NOW()
			FROM matched
			WHERE token.id = matched.token_id
			RETURNING token.id
		)
		SELECT account_id, app_family_id, app_id, token_id, version, kind, status,
		       allow_all, allowed_operations, expires_at, binding_mode
		FROM matched
		WHERE EXISTS (SELECT 1 FROM touched)
	`, appID, tokenHash).Scan(
		&proj.AccountID, &proj.AppFamilyID, &proj.AppID, &proj.TokenID, &proj.Version, &proj.Kind, &proj.AppStatus,
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

func (s *postgresStore) SetAppFamilyBucket(ctx context.Context, appFamilyID, bucketID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
		VALUES ($1, $2)
		ON CONFLICT (app_family_id) DO UPDATE SET
			bucket_id = EXCLUDED.bucket_id,
			updated_at = NOW()
	`, appFamilyID, bucketID)
	if err != nil {
		return fmt.Errorf("set app family bucket: %w", err)
	}
	return nil
}

func (s *postgresStore) GetAppFamilyBucket(ctx context.Context, appFamilyID uuid.UUID) (*AppFamilyBucket, error) {
	var fb AppFamilyBucket
	err := s.db.QueryRow(ctx, `
		SELECT app_family_id, bucket_id, created_at, updated_at
		FROM fused_app_family_buckets
		WHERE app_family_id = $1
	`, appFamilyID).Scan(&fb.AppFamilyID, &fb.BucketID, &fb.CreatedAt, &fb.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBucketNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get app family bucket: %w", err)
	}
	return &fb, nil
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
