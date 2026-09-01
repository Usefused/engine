package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// sdkGenerationBuildSelect retains rows with missing plan authority so scan fails visibly instead of silently stranding a build.
const sdkGenerationBuildSelect = `
	SELECT family.display_name, app.version, app.app_family_id, app.app_id,
	       app.source_hash, COALESCE(app.generator_version, ''),
	       family.target_language, app.selections,
	       COALESCE(plan.resolved_payload->>'description', ''),
	       COALESCE((plan.resolved_payload->>'include_mcp')::boolean, false),
	       COALESCE((plan.resolved_payload->>'skip_sandbox')::boolean, false),
	       COALESCE(plan.resolved_payload->>'default_engine_url', ''),
	       COALESCE(plan.resolved_payload->'contract_bindings', '[]'::jsonb),
	       COALESCE(plan.resolved_payload->'unified_operations', 'null'::jsonb),
	       plan.id, COALESCE(app.sdk_generation_job_id, ''),
	       app.account_id,
	       COALESCE(app.sdk_generation_status, '')
	FROM fused_apps app
	JOIN fused_app_families family
	  ON family.app_family_id = app.app_family_id
	 AND family.account_id = app.account_id
	LEFT JOIN LATERAL (
		SELECT applied.id, applied.resolved_payload
		FROM fused_config_plans applied
		WHERE applied.config_key = app.config_key
		  AND applied.source_hash = app.source_hash
		  AND applied.status = 'applied'
		  AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean, false)
		ORDER BY applied.applied_at DESC, applied.created_at DESC
		LIMIT 1
	) plan ON true
`

// GetSDKGenerationBuild recovers one exact SDK build and its durable Registry
// job identity without admitting the non-runnable app as execution scope.
func (s *postgresStore) GetSDKGenerationBuild(ctx context.Context, accountID, appID uuid.UUID) (*SDKGenerationBuild, error) {
	row := s.db.QueryRow(ctx, sdkGenerationBuildSelect+`
		WHERE app.account_id = $1 AND app.app_id = $2
		  AND family.kind = 'sdk'
	`, accountID, appID)
	build, err := scanSDKGenerationBuild(row)
	// Exact absence remains distinct from a malformed retained build.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get SDK generation build: %w", err)
	}
	return build, nil
}

// ListPendingSDKGenerationBuilds returns one bounded keyset page for startup
// and periodic recovery without loading completed or failed jobs.
func (s *postgresStore) ListPendingSDKGenerationBuilds(ctx context.Context, after uuid.UUID, limit int) ([]SDKGenerationBuild, error) {
	// The package lease batch limit is also a safe upper bound for generation recovery pages.
	if limit <= 0 || limit > models.SDKPackageLeaseBatchLimit {
		limit = models.SDKPackageLeaseBatchLimit
	}
	rows, err := s.db.Query(ctx, sdkGenerationBuildSelect+`
		WHERE family.kind = 'sdk'
		  AND app.status = 'building'
		  AND app.sdk_generation_status = 'pending'
		  AND ($1 = '00000000-0000-0000-0000-000000000000'::uuid OR app.app_id > $1)
		ORDER BY app.app_id
		LIMIT $2
	`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending SDK generation builds: %w", err)
	}
	defer rows.Close()
	builds := make([]SDKGenerationBuild, 0, limit)
	for rows.Next() {
		build, scanErr := scanSDKGenerationBuild(rows)
		// One malformed build invalidates the page so the worker never skips it silently.
		if scanErr != nil {
			return nil, fmt.Errorf("scan pending SDK generation build: %w", scanErr)
		}
		builds = append(builds, *build)
	}
	return builds, rows.Err()
}

type sdkGenerationBuildScanner interface {
	Scan(...any) error
}

// scanSDKGenerationBuild decodes the shared exact and paginated build query
// while keeping its JSON validation identical for both callers.
func scanSDKGenerationBuild(row sdkGenerationBuildScanner) (*SDKGenerationBuild, error) {
	var build SDKGenerationBuild
	var selections, bindings, unifiedOperations []byte
	var planID uuid.UUID
	err := row.Scan(
		&build.Request.Name, &build.Request.Version, &build.Request.AppFamilyID, &build.Request.AppID,
		&build.Request.SourceHash, &build.Request.GeneratorVersion, &build.Request.TargetLanguage,
		&selections, &build.Request.Description, &build.Request.IncludeMCP, &build.Request.SkipSandbox,
		&build.Request.DefaultEngineURL, &bindings, &unifiedOperations, &planID, &build.JobID, &build.AccountID, &build.Status,
	)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(selections, &build.Request.Selections); err != nil {
		return nil, fmt.Errorf("decode SDK package selections: %w", err)
	}
	if err := json.Unmarshal(bindings, &build.Request.ContractBindings); err != nil {
		return nil, fmt.Errorf("decode SDK package contract bindings: %w", err)
	}
	// A null descriptor means the immutable SDK has no Unified operations.
	if string(unifiedOperations) != "null" {
		if err := json.Unmarshal(unifiedOperations, &build.Request.UnifiedOperations); err != nil {
			return nil, fmt.Errorf("decode SDK package unified operations: %w", err)
		}
	}
	build.Request.IdempotencyKey = planID.String()
	build.Request.TargetType = AppKindSDK.String()
	return &build, nil
}

// CompleteSDKGeneration atomically promotes only the still-pending matching
// SDK job, preventing a late old worker from activating a newer retry.
func (s *postgresStore) CompleteSDKGeneration(ctx context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	return s.transitionSDKGeneration(ctx, appID, jobID, idempotencyKey, models.SDKGenerationStatusComplete)
}

// FailSDKGeneration records a terminal package failure while retaining the
// immutable building version for explicit replan and retry.
func (s *postgresStore) FailSDKGeneration(ctx context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	return s.transitionSDKGeneration(ctx, appID, jobID, idempotencyKey, models.SDKGenerationStatusFailed)
}

// sdkGenerationTransitionSQL binds terminal state to the newest applied non-noop plan so an old attempt cannot finish a reclaimed job.
const sdkGenerationTransitionSQL = `
		UPDATE fused_apps
		SET status = $3,
		    sdk_generation_status = $4,
		    activated_at = CASE WHEN $3 = 'active' THEN COALESCE(activated_at, NOW()) ELSE activated_at END
		WHERE app_id = $1
		  AND sdk_generation_job_id = $2
		  AND status = 'building'
		  AND sdk_generation_status = 'pending'
		  AND $5 = (
			SELECT plan.id
			FROM fused_config_plans plan
			WHERE plan.config_key = fused_apps.config_key
			  AND plan.source_hash = fused_apps.source_hash
			  AND plan.status = 'applied'
			  AND NOT COALESCE((plan.resolved_payload->>'noop')::boolean, false)
			ORDER BY plan.applied_at DESC, plan.created_at DESC
			LIMIT 1
		  )`

// transitionSDKGeneration applies one compare-and-swap generation outcome so
// concurrent status checks and background recovery remain idempotent.
func (s *postgresStore) transitionSDKGeneration(ctx context.Context, appID uuid.UUID, jobID, idempotencyKey, terminal string) (bool, error) {
	planID, err := uuid.Parse(idempotencyKey)
	// The applied plan identity fences a retried Registry job whose durable job ID is intentionally reused.
	if err != nil {
		return false, errors.New("transition SDK generation: invalid generation attempt identity")
	}
	status := AppStatusBuilding
	// Only confirmed completion makes an SDK version runnable; failure stays building.
	if terminal == models.SDKGenerationStatusComplete {
		status = AppStatusActive
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("transition SDK generation: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	// Completion reacquires quota under the singleton entitlement lock so two
	// building families cannot become runnable past the workspace limit.
	if status == AppStatusActive {
		if err := admitSDKGenerationActivation(ctx, tx, appID); err != nil {
			return false, err
		}
	}
	result, err := tx.Exec(ctx, sdkGenerationTransitionSQL, appID, jobID, status, terminal, planID)
	if err != nil {
		return false, fmt.Errorf("transition SDK generation: %w", err)
	}
	changed := result.RowsAffected() == 1
	// An idempotent loser commits no data change but still closes its transaction cleanly.
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("transition SDK generation: commit: %w", err)
	}
	return changed, nil
}

// admitSDKGenerationActivation serializes the invokable-family count and
// permits an already-runnable sibling family without allocating another unit.
func admitSDKGenerationActivation(ctx context.Context, tx pgx.Tx, appID uuid.UUID) error {
	var accountID, familyID uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT account_id, app_family_id
		FROM fused_apps
		WHERE app_id = $1
	`, appID).Scan(&accountID, &familyID)
	// Completion must bind quota admission to the exact retained app before counting its family.
	if err != nil {
		return fmt.Errorf("activate SDK generation: load app identity: %w", err)
	}
	return admitSDKFamilyActivation(ctx, tx, accountID, familyID)
}

// admitSDKFamilyActivation serializes every path that can make an SDK family invokable, including cache-hit apply and background completion.
func admitSDKFamilyActivation(ctx context.Context, tx pgx.Tx, accountID, familyID uuid.UUID) error {
	var limit int
	err := tx.QueryRow(ctx, `
		SELECT max_sdk_families
		FROM fused_runtime_entitlements
		WHERE singleton_key = 1
		FOR UPDATE
	`).Scan(&limit)
	// Missing entitlement state cannot safely turn a non-runnable version active.
	if err != nil {
		return fmt.Errorf("activate SDK generation: load entitlement: %w", err)
	}
	// A negative limit is explicitly unlimited and needs no count query.
	if limit < 0 {
		return nil
	}
	var current int
	var targetInvokable bool
	var targetExists bool
	err = tx.QueryRow(ctx, `
		WITH families AS (
			SELECT family.app_family_id,
			       EXISTS (
				 SELECT 1 FROM fused_apps app
				 WHERE app.app_family_id = family.app_family_id
				   AND app.account_id = family.account_id
				   AND app.status IN ('active', 'deprecated')
			       ) AS invokable
			FROM fused_app_families family
			WHERE family.account_id = $1
			  AND family.kind = 'sdk'
		)
		SELECT COUNT(*) FILTER (WHERE invokable),
		       COALESCE(BOOL_OR(app_family_id = $2 AND invokable), FALSE),
		       COALESCE(BOOL_OR(app_family_id = $2), FALSE)
		FROM families
	`, accountID, familyID).Scan(&current, &targetInvokable, &targetExists)
	if err != nil {
		return fmt.Errorf("activate SDK generation: count families: %w", err)
	}
	// A missing or cross-account target family cannot consume or inherit quota.
	if !targetExists {
		return errors.New("activate SDK generation: target family not found")
	}
	// A sibling active version means this family already owns its quota unit.
	if targetInvokable {
		return nil
	}
	if current >= limit {
		return ErrSDKFamilyLimitExceeded
	}
	return nil
}
