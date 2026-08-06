package migration

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// ApplyReport records only safe counts. Existing token hashes and selections
// are migrated inside PostgreSQL and never returned by the administrative API.
type ApplyReport struct {
	AppliedFamilies int `json:"applied_families"`
	MigratedApps    int `json:"migrated_apps"`
	MigratedTokens  int `json:"migrated_tokens"`
	SkippedGroups   int `json:"skipped_groups"`
}

type applyCandidate struct {
	AccountID      uuid.UUID
	Kind           string
	CanonicalName  string
	DisplayName    string
	TargetLanguage string
	OwnerSubjectID *uuid.UUID
	OwnerTeamID    *uuid.UUID
	ArtifactID     uuid.UUID
	Name           string
	Version        string
	ConfigKey      string
	SourceHash     string
}

// Apply migrates every conflict-free dry-run group in one transaction. The
// temporary candidate table lets PostgreSQL perform relationship moves with a
// bounded statement count instead of issuing queries per family or app.
func Apply(ctx context.Context, db *pgxpool.Pool) (*ApplyReport, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.migration.app_family.apply")
	defer span.End()

	dryRun, err := DryRun(ctx, db)
	if err != nil {
		return nil, err
	}
	candidates, skipped := eligibleCandidates(dryRun)
	report := &ApplyReport{SkippedGroups: skipped}
	if skipped != 0 {
		return nil, fmt.Errorf("app-family migration: %d conflicted families require an explicit split", skipped)
	}
	if len(candidates) == 0 {
		if err := dropEmptyLegacyAppTables(ctx, db); err != nil {
			return nil, err
		}
		return report, nil
	}
	if err := applyCandidates(ctx, db, candidates, report); err != nil {
		return nil, err
	}
	span.SetAttributes(
		attribute.Int("family_count", report.AppliedFamilies),
		attribute.Int("app_count", report.MigratedApps),
		attribute.Int("token_count", report.MigratedTokens),
		attribute.Int("skipped_group_count", report.SkippedGroups),
	)
	return report, nil
}

func dropEmptyLegacyAppTables(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app-family migration: begin empty cleanup: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := dropLegacyAppTables(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app-family migration: commit empty cleanup: %w", err)
	}
	return nil
}

func eligibleCandidates(report *DryRunReport) ([]applyCandidate, int) {
	var candidates []applyCandidate
	skipped := 0
	for _, family := range report.Families {
		if len(family.Conflicts) != 0 {
			skipped++
			continue
		}
		for _, member := range family.Members {
			candidates = append(candidates, applyCandidate{
				AccountID: family.AccountID, Kind: family.Kind,
				CanonicalName: family.CanonicalName, DisplayName: family.DisplayName,
				TargetLanguage: family.TargetLanguage,
				OwnerSubjectID: family.OwnerSubjectID, OwnerTeamID: family.OwnerTeamID,
				ArtifactID: member.ArtifactID, Name: member.Name, Version: member.Version,
				ConfigKey: member.ConfigKey, SourceHash: member.SourceHash,
			})
		}
	}
	return candidates, skipped
}

func applyCandidates(ctx context.Context, db *pgxpool.Pool, candidates []applyCandidate, report *ApplyReport) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("app-family migration: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := createCandidateTable(ctx, tx); err != nil {
		return err
	}
	if err := copyCandidates(ctx, tx, candidates); err != nil {
		return err
	}
	if err := validateCandidates(ctx, tx); err != nil {
		return err
	}
	if err := migrateFamiliesAndApps(ctx, tx, report); err != nil {
		return err
	}
	if err := validateMigratedRelationships(ctx, tx); err != nil {
		return err
	}
	if err := journalMigration(ctx, tx); err != nil {
		return err
	}
	if err := dropLegacyAppTables(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("app-family migration: commit: %w", err)
	}
	return nil
}

func dropLegacyAppTables(ctx context.Context, tx pgx.Tx) error {
	// Legacy rows are removed only after every relationship has been validated
	// in this transaction; a failed drop rolls the complete cutover back.
	_, err := tx.Exec(ctx, `
		DROP TABLE IF EXISTS fused_artifact_tokens;
		DROP TABLE IF EXISTS fused_artifact_buckets;
		DROP TABLE IF EXISTS fused_artifact_snapshots;
		DROP TABLE IF EXISTS fused_artifact_scopes;
	`)
	if err != nil {
		return fmt.Errorf("app-family migration: drop legacy app tables: %w", err)
	}
	return nil
}

func createCandidateTable(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		CREATE TEMP TABLE app_family_candidates (
			account_id uuid NOT NULL, kind text NOT NULL, canonical_name text NOT NULL,
			display_name text NOT NULL, target_language text NOT NULL,
			owner_subject_id uuid, owner_team_id uuid, artifact_id uuid PRIMARY KEY,
			name text NOT NULL, version text NOT NULL, config_key text NOT NULL,
			source_hash text NOT NULL
		) ON COMMIT DROP
	`)
	if err != nil {
		return fmt.Errorf("app-family migration: create candidates: %w", err)
	}
	return nil
}

func copyCandidates(ctx context.Context, tx pgx.Tx, candidates []applyCandidate) error {
	rows := make([][]any, 0, len(candidates))
	for _, candidate := range candidates {
		rows = append(rows, []any{
			candidate.AccountID, candidate.Kind, candidate.CanonicalName,
			candidate.DisplayName, candidate.TargetLanguage,
			candidate.OwnerSubjectID, candidate.OwnerTeamID, candidate.ArtifactID,
			candidate.Name, candidate.Version, candidate.ConfigKey, candidate.SourceHash,
		})
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{"app_family_candidates"}, []string{
		"account_id", "kind", "canonical_name", "display_name", "target_language",
		"owner_subject_id", "owner_team_id", "artifact_id", "name", "version",
		"config_key", "source_hash",
	}, pgx.CopyFromRows(rows))
	if err != nil {
		return fmt.Errorf("app-family migration: copy candidates: %w", err)
	}
	return nil
}

func validateCandidates(ctx context.Context, tx pgx.Tx) error {
	var invalid int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM app_family_candidates candidate
		LEFT JOIN fused_artifact_scopes scope
		  ON scope.artifact_id = candidate.artifact_id
		LEFT JOIN fused_config_states state
		  ON state.config_key = scope.config_key
		 AND state.config_type = scope.kind
		 AND state.latest_resource_id = scope.artifact_id
		WHERE scope.artifact_id IS NULL OR state.id IS NULL
		   OR scope.account_id <> candidate.account_id OR scope.kind <> candidate.kind
		   OR scope.name IS DISTINCT FROM candidate.name
		   OR scope.version IS DISTINCT FROM candidate.version
		   OR scope.config_key IS DISTINCT FROM candidate.config_key
		   OR state.source_hash <> candidate.source_hash
		   OR state.desired_state->>'kind' IS DISTINCT FROM candidate.kind
		   OR state.desired_state->>'name' IS DISTINCT FROM candidate.name
		   OR state.desired_state->>'version' IS DISTINCT FROM candidate.version
	`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("app-family migration: validate candidates: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("app-family migration: %d candidates changed after dry-run", invalid)
	}
	return nil
}

func migrateFamiliesAndApps(ctx context.Context, tx pgx.Tx, report *ApplyReport) error {
	families, err := execCount(ctx, tx, "families", migrateFamiliesSQL)
	if err != nil {
		return err
	}
	apps, err := execCount(ctx, tx, "apps", migrateAppsSQL)
	if err != nil {
		return err
	}
	if _, err := execCount(ctx, tx, "capabilities", migrateCapabilitiesSQL); err != nil {
		return err
	}
	if _, err := execCount(ctx, tx, "tombstones", migrateTombstonesSQL); err != nil {
		return err
	}
	tokens, err := execCount(ctx, tx, "tokens", migrateTokensSQL)
	if err != nil {
		return err
	}
	if _, err := execCount(ctx, tx, "buckets", migrateBucketsSQL); err != nil {
		return err
	}
	report.AppliedFamilies = families
	report.MigratedApps = apps
	report.MigratedTokens = tokens
	return nil
}

func execCount(ctx context.Context, tx pgx.Tx, step, query string) (int, error) {
	tag, err := tx.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("app-family migration %s: %w", step, err)
	}
	return int(tag.RowsAffected()), nil
}

const migrateFamiliesSQL = `
INSERT INTO fused_app_families
	(app_family_id, account_id, kind, canonical_name, display_name, target_language,
	 owner_subject_id, owner_team_id)
SELECT gen_random_uuid(), account_id, kind, canonical_name,
	MIN(display_name), NULLIF(MIN(target_language), ''),
	NULLIF(MIN(COALESCE(owner_subject_id::text, '')), '')::uuid,
	NULLIF(MIN(COALESCE(owner_team_id::text, '')), '')::uuid
FROM app_family_candidates
GROUP BY account_id, kind, canonical_name
ON CONFLICT (account_id, kind, canonical_name) DO NOTHING`

const migrateAppsSQL = `
INSERT INTO fused_apps
	(app_id, app_family_id, account_id, version, config_key, source_hash,
	 capability_hash, scope_schema_version, selections, generator_version,
	 status, created_by, created_at, activated_at)
SELECT scope.artifact_id, family.app_family_id, scope.account_id, scope.version,
	 scope.config_key, state.source_hash,
	 encode(sha256(convert_to(scope.selections::text, 'UTF8')), 'hex'),
	 scope.scope_schema_version, scope.selections,
	 CASE WHEN scope.kind = 'sdk' THEN 'legacy-unpinned' END,
	 'active', state.updated_by, scope.created_at, scope.created_at
FROM app_family_candidates candidate
JOIN fused_artifact_scopes scope ON scope.artifact_id = candidate.artifact_id
JOIN fused_config_states state ON state.config_key = candidate.config_key
JOIN fused_app_families family
	ON family.account_id = candidate.account_id AND family.kind = candidate.kind
	AND family.canonical_name = candidate.canonical_name
WHERE scope.deactivated_at IS NULL
ON CONFLICT (app_id) DO NOTHING`

const migrateCapabilitiesSQL = `
WITH selected AS (
	SELECT app.app_id, selection.value AS selection,
	       'service:' || (selection.value->>'service_id') || ':' || (selection.value->>'service_version_id') AS prefix
	FROM app_family_candidates candidate
	JOIN fused_apps app ON app.app_id = candidate.artifact_id
	CROSS JOIN LATERAL jsonb_array_elements(app.selections) AS selection(value)
	WHERE COALESCE(selection.value->>'service_id', '') <> ''
	  AND COALESCE(selection.value->>'service_version_id', '') <> ''
), capabilities AS (
	SELECT app_id, prefix AS capability_key FROM selected
	UNION
	SELECT app_id, prefix || ':operations:*' FROM selected WHERE COALESCE((selection->>'select_all')::boolean, false)
	UNION
	SELECT app_id, prefix || ':webhooks:*' FROM selected WHERE COALESCE((selection->>'webhook_select_all')::boolean, false)
	UNION
	SELECT app_id, prefix || ':endpoint:' || value
	FROM selected CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(selection->'endpoint_ids', '[]'::jsonb)) AS item(value)
	UNION
	SELECT app_id, prefix || ':operation:' || value
	FROM selected CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(selection->'operation_names', '[]'::jsonb)) AS item(value)
	UNION
	SELECT app_id, prefix || ':webhook:' || value
	FROM selected CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(selection->'webhook_ids', '[]'::jsonb)) AS item(value)
	UNION
	SELECT app_id, prefix || ':webhook-name:' || value
	FROM selected CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(selection->'webhook_names', '[]'::jsonb)) AS item(value)
	UNION
	SELECT app_id, prefix || ':connect-scope:' || value
	FROM selected CROSS JOIN LATERAL jsonb_array_elements_text(COALESCE(selection->'connect_scopes', '[]'::jsonb)) AS item(value)
	UNION
	SELECT app_id, prefix || ':auth:' || (selection->>'auth_type') || ':' || COALESCE(selection->>'auth_name', '')
	FROM selected WHERE COALESCE(selection->>'auth_type', '') <> ''
	UNION
	SELECT app_id, prefix || ':injection:' || (injection.value->>'location') || ':' ||
	       (injection.value->>'name') || ':' || COALESCE(injection.value->>'mode', '')
	FROM selected
	CROSS JOIN LATERAL jsonb_array_elements(COALESCE(selection->'injections', '[]'::jsonb)) AS injection(value)
	WHERE COALESCE(injection.value->>'location', '') <> '' AND COALESCE(injection.value->>'name', '') <> ''
)
INSERT INTO fused_app_capabilities (app_id, capability_key)
SELECT app_id, capability_key FROM capabilities WHERE capability_key <> ''
ON CONFLICT (app_id, capability_key) DO NOTHING`

const migrateTombstonesSQL = `
INSERT INTO fused_app_tombstones
	(app_id, app_family_id, account_id, version, source_hash, deactivated_at)
SELECT scope.artifact_id, family.app_family_id, scope.account_id, scope.version,
	 state.source_hash, scope.deactivated_at
FROM app_family_candidates candidate
JOIN fused_artifact_scopes scope ON scope.artifact_id = candidate.artifact_id
JOIN fused_config_states state ON state.config_key = candidate.config_key
JOIN fused_app_families family
	ON family.account_id = candidate.account_id AND family.kind = candidate.kind
	AND family.canonical_name = candidate.canonical_name
WHERE scope.deactivated_at IS NOT NULL
ON CONFLICT (app_family_id, version) DO NOTHING`

const migrateTokensSQL = `
INSERT INTO fused_app_tokens (id, app_family_id, token_hash, name, last_used_at, created_at)
SELECT token.id, family.app_family_id, token.token_hash, token.name,
	 token.last_used_at, token.created_at
FROM fused_artifact_tokens token
JOIN app_family_candidates candidate ON candidate.artifact_id = token.artifact_id
JOIN fused_app_families family
	ON family.account_id = candidate.account_id AND family.kind = candidate.kind
	AND family.canonical_name = candidate.canonical_name
ON CONFLICT (token_hash) DO NOTHING`

const migrateBucketsSQL = `
INSERT INTO fused_app_family_buckets (app_family_id, bucket_id)
SELECT DISTINCT family.app_family_id, bucket.bucket_id
FROM fused_artifact_buckets bucket
JOIN app_family_candidates candidate ON candidate.artifact_id = bucket.artifact_id
JOIN fused_app_families family
	ON family.account_id = candidate.account_id AND family.kind = candidate.kind
	AND family.canonical_name = candidate.canonical_name
ON CONFLICT (app_family_id) DO NOTHING`

func validateMigratedRelationships(ctx context.Context, tx pgx.Tx) error {
	var invalid int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM app_family_candidates candidate
		JOIN fused_app_families family
		  ON family.account_id = candidate.account_id AND family.kind = candidate.kind
		 AND family.canonical_name = candidate.canonical_name
		LEFT JOIN fused_apps app ON app.app_id = candidate.artifact_id
		LEFT JOIN fused_app_tombstones tombstone ON tombstone.app_id = candidate.artifact_id
		WHERE (app.app_id IS NULL) = (tombstone.app_id IS NULL)
		   OR COALESCE(app.app_family_id, tombstone.app_family_id) <> family.app_family_id
		   OR COALESCE(app.source_hash, tombstone.source_hash) <> candidate.source_hash
	`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("app-family migration: validate relationships: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("app-family migration: %d app identities failed validation", invalid)
	}
	if err := validateTokenRelationships(ctx, tx); err != nil {
		return err
	}
	return validateBucketRelationships(ctx, tx)
}

func validateBucketRelationships(ctx context.Context, tx pgx.Tx) error {
	var invalid int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM fused_artifact_buckets source
		JOIN app_family_candidates candidate ON candidate.artifact_id = source.artifact_id
		JOIN fused_app_families family
		  ON family.account_id = candidate.account_id AND family.kind = candidate.kind
		 AND family.canonical_name = candidate.canonical_name
		LEFT JOIN fused_app_family_buckets target
		  ON target.app_family_id = family.app_family_id AND target.bucket_id = source.bucket_id
		WHERE target.app_family_id IS NULL
	`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("app-family migration: validate buckets: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("app-family migration: %d bucket mappings failed validation", invalid)
	}
	return nil
}

func validateTokenRelationships(ctx context.Context, tx pgx.Tx) error {
	var invalid int
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM fused_artifact_tokens source
		JOIN app_family_candidates candidate ON candidate.artifact_id = source.artifact_id
		JOIN fused_app_families family
		  ON family.account_id = candidate.account_id AND family.kind = candidate.kind
		 AND family.canonical_name = candidate.canonical_name
		LEFT JOIN fused_app_tokens target
		  ON target.token_hash = source.token_hash AND target.app_family_id = family.app_family_id
		WHERE target.id IS NULL
	`).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("app-family migration: validate tokens: %w", err)
	}
	if invalid != 0 {
		return fmt.Errorf("app-family migration: %d tokens failed validation", invalid)
	}
	return nil
}

func journalMigration(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO fused_app_family_migrations
			(account_id, kind, canonical_name, app_family_id, migrated_apps, migrated_tokens)
		SELECT candidate.account_id, candidate.kind, candidate.canonical_name,
		       family.app_family_id,
		       COUNT(DISTINCT candidate.artifact_id)::integer,
		       COUNT(DISTINCT token.id)::integer
		FROM app_family_candidates candidate
		JOIN fused_app_families family
		  ON family.account_id = candidate.account_id AND family.kind = candidate.kind
		 AND family.canonical_name = candidate.canonical_name
		LEFT JOIN fused_artifact_tokens token ON token.artifact_id = candidate.artifact_id
		GROUP BY candidate.account_id, candidate.kind, candidate.canonical_name, family.app_family_id
		ON CONFLICT (account_id, kind, canonical_name) DO UPDATE SET
			app_family_id = EXCLUDED.app_family_id,
			migrated_apps = EXCLUDED.migrated_apps,
			migrated_tokens = EXCLUDED.migrated_tokens,
			completed_at = NOW()
	`)
	if err != nil {
		return fmt.Errorf("app-family migration: journal: %w", err)
	}
	return nil
}
