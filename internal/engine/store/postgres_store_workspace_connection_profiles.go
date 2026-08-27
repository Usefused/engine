package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type workspaceBindingWriteRow struct {
	SourceKind            string   `json:"source_kind"`
	LiteralValue          *string  `json:"literal_value"`
	SourcePath            *string  `json:"source_path"`
	TargetLocation        string   `json:"target_location"`
	TargetName            string   `json:"target_name"`
	OperationIDs          []string `json:"operation_ids"`
	Mode                  string   `json:"mode"`
	Provenance            string   `json:"provenance"`
	SourceProfileRevision *int     `json:"source_profile_revision"`
}

type workspaceProfileBatchWriteRow struct {
	ServiceID         uuid.UUID                  `json:"service_id"`
	ServiceVersionID  uuid.UUID                  `json:"service_version_id"`
	AuthType          string                     `json:"auth_type"`
	Layer             string                     `json:"layer"`
	RegistryProfileID *uuid.UUID                 `json:"registry_profile_id"`
	ProfileRevision   int                        `json:"profile_revision"`
	ProfileHash       string                     `json:"profile_hash"`
	Provenance        string                     `json:"provenance"`
	ProfileSnapshot   json.RawMessage            `json:"profile_snapshot"`
	Bindings          []workspaceBindingWriteRow `json:"bindings"`
}

// ReconcileWorkspaceProfiles applies all replacements and deletes in a fixed
// number of SQL statements and one transaction, avoiding per-version round
// trips and partially applied workspace configuration.
func (s *postgresStore) ReconcileWorkspaceProfiles(ctx context.Context, replacements []WorkspaceProfileReplacement, deletes []WorkspaceProfileRef) error {
	// Validate the complete batch before opening a transaction or changing routing state.
	for _, replacement := range replacements {
		if err := validateWorkspaceProfileWrite(replacement.Profile, replacement.Bindings); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := deleteWorkspaceProfilesBatch(ctx, tx, deletes); err != nil {
		return err
	}
	if err := replaceWorkspaceProfilesBatch(ctx, tx, replacements); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deleteWorkspaceProfilesBatch sends exact composite identities to SQL so rows
// outside the planned service versions cannot be detached accidentally. Only
// override rows are ever targeted here -- a baseline is a pinned publication,
// never removed by a plan "delete" action.
func deleteWorkspaceProfilesBatch(ctx context.Context, tx pgx.Tx, refs []WorkspaceProfileRef) error {
	// Empty delete sets intentionally avoid a pointless database round trip.
	if len(refs) == 0 {
		return nil
	}
	serviceIDs, versionIDs, authTypes := workspaceProfileRefArrays(refs)
	_, err := tx.Exec(ctx, `
		WITH requested AS (
			SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::text[])
			AS input(service_id, service_version_id, auth_type)
		)
		DELETE FROM fused_workspace_connection_profiles profiles
		USING requested
		WHERE profiles.service_id = requested.service_id
		  AND profiles.service_version_id = requested.service_version_id
		  AND profiles.auth_type = requested.auth_type
		  AND profiles.layer = 'override'`, serviceIDs, versionIDs, authTypes)
	return err
}

// replaceWorkspaceProfilesBatch uses nested JSON recordsets to upsert profile
// layers, remove their bindings, and insert replacements server-side.
func replaceWorkspaceProfilesBatch(ctx context.Context, tx pgx.Tx, replacements []WorkspaceProfileReplacement) error {
	// Empty replacement sets leave every other workspace tuple untouched.
	if len(replacements) == 0 {
		return nil
	}
	payload, err := json.Marshal(workspaceProfileBatchWriteRows(replacements))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH input AS (
			SELECT * FROM jsonb_to_recordset($1::jsonb) AS rows(
				service_id uuid, service_version_id uuid,
				auth_type text, layer text, registry_profile_id uuid, profile_revision integer,
				profile_hash text, provenance text, profile_snapshot jsonb, bindings jsonb
			)
		), upserted AS (
			INSERT INTO fused_workspace_connection_profiles (
				service_id, service_version_id, auth_type, layer,
				registry_profile_id, profile_revision, profile_hash, provenance, profile_snapshot
			)
			SELECT service_id, service_version_id, auth_type, layer,
			       registry_profile_id, profile_revision, profile_hash, provenance, profile_snapshot
			FROM input
			ON CONFLICT ON CONSTRAINT uq_fused_workspace_connection_profile DO UPDATE SET
				registry_profile_id = EXCLUDED.registry_profile_id,
				profile_revision = EXCLUDED.profile_revision,
				profile_hash = EXCLUDED.profile_hash,
				provenance = EXCLUDED.provenance,
				profile_snapshot = EXCLUDED.profile_snapshot,
				updated_at = NOW()
			RETURNING id, service_id, service_version_id, auth_type, layer
		), deleted AS (
			DELETE FROM fused_workspace_connection_bindings bindings
			USING upserted profiles
			WHERE bindings.profile_id = profiles.id
			RETURNING bindings.id
		), deletion_barrier AS (
			SELECT COUNT(*) FROM deleted
		)
		INSERT INTO fused_workspace_connection_bindings (
			service_id, service_version_id, profile_id,
			source_kind, literal_value, source_path, target_location, target_name,
			operation_ids, mode, provenance, source_profile_revision
		)
		SELECT profiles.service_id, profiles.service_version_id, profiles.id,
		       bindings.source_kind, bindings.literal_value, bindings.source_path, bindings.target_location,
		       NULLIF(bindings.target_name, ''), bindings.operation_ids, bindings.mode,
		       bindings.provenance, bindings.source_profile_revision
		FROM upserted profiles
		JOIN input rows ON rows.service_id = profiles.service_id
		 AND rows.service_version_id = profiles.service_version_id
		 AND rows.auth_type = profiles.auth_type
		 AND rows.layer = profiles.layer
		CROSS JOIN deletion_barrier
		CROSS JOIN LATERAL jsonb_to_recordset(COALESCE(rows.bindings, '[]'::jsonb)) AS bindings(
			source_kind text, literal_value text, source_path text, target_location text,
			target_name text, operation_ids text[], mode text, provenance text,
			source_profile_revision integer
		)`, payload)
	return err
}

// workspaceProfileBatchWriteRows serializes only validated profile and
// binding data; nested rows let PostgreSQL perform one set-based replacement.
func workspaceProfileBatchWriteRows(replacements []WorkspaceProfileReplacement) []workspaceProfileBatchWriteRow {
	rows := make([]workspaceProfileBatchWriteRow, 0, len(replacements))
	for _, replacement := range replacements {
		profile := replacement.Profile
		rows = append(rows, workspaceProfileBatchWriteRow{
			ServiceID: profile.ServiceID, ServiceVersionID: profile.ServiceVersionID,
			AuthType: profile.AuthType, Layer: profile.Layer, RegistryProfileID: profile.RegistryProfileID,
			ProfileRevision: profile.ProfileRevision, ProfileHash: profile.ProfileHash,
			Provenance: profile.Provenance, ProfileSnapshot: json.RawMessage(profile.ProfileSnapshot),
			Bindings: workspaceBindingWriteRows(replacement.Bindings),
		})
	}
	return rows
}

// UpsertWorkspaceProfileOverride swaps the override layer and its bindings in
// one transaction. Callers never choose between create and update -- this is
// always an upsert of the workspace-authored override, per the plan's
// product rule that editing an effective profile performs an upsert.
func (s *postgresStore) UpsertWorkspaceProfileOverride(ctx context.Context, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) (*WorkspaceConnectionProfile, error) {
	profile.Layer = "override"
	profile.Provenance = "workspace"
	profile.RegistryProfileID = nil
	if err := validateWorkspaceProfileWrite(profile, bindings); err != nil {
		return nil, err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	stored, err := upsertWorkspaceProfileRow(ctx, tx, profile)
	if err != nil {
		return nil, err
	}
	if err := replaceWorkspaceProfileBindings(ctx, tx, stored.ID, profile, bindings); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return stored, nil
}

// validateWorkspaceProfileWrite checks ownership and layer invariants before
// either single or batch SQL can persist runtime routing behavior.
func validateWorkspaceProfileWrite(profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) error {
	if !completeWorkspaceProfileIdentity(profile) {
		return errors.New("workspace profile identity is required")
	}
	if !completeWorkspaceProfileRevision(profile) {
		return errors.New("workspace profile revision, hash, and snapshot are required")
	}
	if err := validateWorkspaceProfileLayer(profile); err != nil {
		return err
	}
	for _, binding := range bindings {
		if !workspaceBindingBelongsToProfile(binding, profile) {
			return errors.New("workspace binding ownership does not match profile")
		}
		if err := validateWorkspaceBindingSource(binding); err != nil {
			return err
		}
	}
	return nil
}

// validateWorkspaceProfileLayer keeps publication identity tied to the layer:
// baselines remain auditable to Registry while local overrides cannot claim a
// Registry publication they do not own.
func validateWorkspaceProfileLayer(profile WorkspaceConnectionProfile) error {
	switch profile.Layer {
	case "baseline":
		if profile.RegistryProfileID == nil {
			return errors.New("workspace baseline profile requires a registry profile ID")
		}
	case "override":
		if profile.RegistryProfileID != nil {
			return errors.New("workspace override profile cannot reference a registry profile ID")
		}
	default:
		return errors.New("workspace profile layer must be baseline or override")
	}
	return nil
}

// completeWorkspaceProfileIdentity keeps the composite tenant/version key in
// one place so batch and single-row writers enforce the same ownership tuple.
func completeWorkspaceProfileIdentity(profile WorkspaceConnectionProfile) bool {
	return profile.ServiceID != uuid.Nil && profile.ServiceVersionID != uuid.Nil && profile.AuthType != ""
}

// completeWorkspaceProfileRevision prevents unhashed or empty snapshots from
// becoming a runtime profile that cannot be audited back to configuration.
func completeWorkspaceProfileRevision(profile WorkspaceConnectionProfile) bool {
	return profile.ProfileRevision > 0 && profile.ProfileHash != "" && len(profile.ProfileSnapshot) > 0
}

// workspaceBindingBelongsToProfile rejects mixed batches before SQL derives
// stored ownership from the profile row.
func workspaceBindingBelongsToProfile(binding WorkspaceConnectionBinding, profile WorkspaceConnectionProfile) bool {
	return binding.ServiceID == profile.ServiceID && binding.ServiceVersionID == profile.ServiceVersionID
}

// validateWorkspaceBindingSource enforces the closed literal-or-resource
// source model so runtime never has to guess between competing values.
func validateWorkspaceBindingSource(binding WorkspaceConnectionBinding) error {
	literal := binding.SourceKind == "literal" && binding.LiteralValue != nil && binding.SourcePath == nil
	dynamic := binding.SourceKind == "connection_resource" && binding.LiteralValue == nil && binding.SourcePath != nil
	if !literal && !dynamic {
		return errors.New("workspace binding must declare exactly one value source")
	}
	return nil
}

// upsertWorkspaceProfileRow preserves stable profile identity while replacing
// its immutable behavior snapshot and revision metadata.
func upsertWorkspaceProfileRow(ctx context.Context, tx pgx.Tx, profile WorkspaceConnectionProfile) (*WorkspaceConnectionProfile, error) {
	query := `
		INSERT INTO fused_workspace_connection_profiles (
			service_id, service_version_id, auth_type, layer,
			registry_profile_id, profile_revision, profile_hash, provenance, profile_snapshot
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT ON CONSTRAINT uq_fused_workspace_connection_profile DO UPDATE SET
			registry_profile_id = EXCLUDED.registry_profile_id,
			profile_revision = EXCLUDED.profile_revision,
			profile_hash = EXCLUDED.profile_hash,
			provenance = EXCLUDED.provenance,
			profile_snapshot = EXCLUDED.profile_snapshot,
			updated_at = NOW()
		RETURNING id, service_id, service_version_id, auth_type, layer,
		          registry_profile_id, profile_revision, profile_hash, provenance,
		          profile_snapshot, is_public, created_at, updated_at`
	row := tx.QueryRow(ctx, query, profile.ServiceID, profile.ServiceVersionID,
		profile.AuthType, profile.Layer, profile.RegistryProfileID,
		profile.ProfileRevision, profile.ProfileHash, profile.Provenance, profile.ProfileSnapshot)
	return scanWorkspaceProfile(row)
}

// replaceWorkspaceProfileBindings replaces every binding owned by one profile
// row. Bindings are wholly owned by their profile layer (no per-binding
// override flag survives a rewrite), so the safe operation is delete-then-
// insert inside the same transaction as the profile upsert.
func replaceWorkspaceProfileBindings(ctx context.Context, tx pgx.Tx, profileID uuid.UUID, profile WorkspaceConnectionProfile, bindings []WorkspaceConnectionBinding) error {
	if _, err := tx.Exec(ctx, `DELETE FROM fused_workspace_connection_bindings WHERE profile_id = $1`, profileID); err != nil {
		return err
	}
	if len(bindings) == 0 {
		return nil
	}
	payload, err := json.Marshal(workspaceBindingWriteRows(bindings))
	if err != nil {
		return err
	}
	query := `
		INSERT INTO fused_workspace_connection_bindings (
			service_id, service_version_id, profile_id,
			source_kind, literal_value, source_path, target_location, target_name,
			operation_ids, mode, provenance, source_profile_revision
		)
		SELECT $1,$2,$3, rows.source_kind, rows.literal_value, rows.source_path,
		       rows.target_location, NULLIF(rows.target_name, ''), rows.operation_ids,
		       rows.mode, rows.provenance, rows.source_profile_revision
		FROM jsonb_to_recordset($4::jsonb) AS rows(
			source_kind text, literal_value text, source_path text, target_location text,
			target_name text, operation_ids text[], mode text, provenance text,
			source_profile_revision integer
		)`
	_, err = tx.Exec(ctx, query, profile.ServiceID, profile.ServiceVersionID, profileID, payload)
	return err
}

// workspaceBindingWriteRows projects only columns accepted by set-based JSON inserts.
func workspaceBindingWriteRows(bindings []WorkspaceConnectionBinding) []workspaceBindingWriteRow {
	rows := make([]workspaceBindingWriteRow, 0, len(bindings))
	for _, binding := range bindings {
		rows = append(rows, workspaceBindingWriteRow{
			SourceKind: binding.SourceKind, LiteralValue: binding.LiteralValue, SourcePath: binding.SourcePath,
			TargetLocation: binding.TargetLocation, TargetName: binding.TargetName, OperationIDs: nonNilStrings(binding.OperationIDs),
			Mode: binding.Mode, Provenance: binding.Provenance, SourceProfileRevision: binding.SourceProfileRevision,
		})
	}
	return rows
}

// ResetWorkspaceProfile deletes only the override layer (and, via FK cascade,
// its bindings); the baseline row -- if any -- is left untouched so it
// immediately becomes the new effective profile without a Registry call.
func (s *postgresStore) ResetWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error {
	_, err := s.db.Exec(ctx, `
		DELETE FROM fused_workspace_connection_profiles
		WHERE service_id = $1 AND service_version_id = $2 AND auth_type = $3 AND layer = 'override'`,
		serviceID, serviceVersionID, authType)
	return err
}

// GetEffectiveWorkspaceProfile resolves override-if-present-else-baseline
// precedence in SQL (ORDER BY layer DESC puts 'override' before 'baseline',
// LIMIT 1 picks the winner) instead of loading both layers into Go and
// picking one in application code, per the plan's explicit requirement.
func (s *postgresStore) GetEffectiveWorkspaceProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) (*WorkspaceConnectionProfile, error) {
	query := `
		SELECT id, service_id, service_version_id, auth_type, layer,
		       registry_profile_id, profile_revision, profile_hash, provenance,
		       profile_snapshot, is_public, created_at, updated_at
		FROM fused_workspace_connection_profiles
		WHERE service_id = $1 AND service_version_id = $2 AND auth_type = $3
		ORDER BY layer DESC
		LIMIT 1`
	profile, err := scanWorkspaceProfile(s.db.QueryRow(ctx, query, serviceID, serviceVersionID, authType))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return profile, err
}

// GetEffectiveWorkspaceProfiles resolves many tuples' effective profile in one
// query using DISTINCT ON, keeping the same override-wins-over-baseline
// precedence as the single-tuple read without a broad load-then-filter-in-Go
// pass.
func (s *postgresStore) GetEffectiveWorkspaceProfiles(ctx context.Context, refs []WorkspaceProfileRef) ([]WorkspaceConnectionProfile, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	serviceIDs, versionIDs, authTypes := workspaceProfileRefArrays(refs)
	rows, err := s.db.Query(ctx, `
		WITH requested AS (
			SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::text[])
			AS input(service_id, service_version_id, auth_type)
		)
		SELECT DISTINCT ON (profiles.service_id, profiles.service_version_id, profiles.auth_type)
		       profiles.id, profiles.service_id,
		       profiles.service_version_id, profiles.auth_type, profiles.layer, profiles.registry_profile_id,
		       profiles.profile_revision, profiles.profile_hash, profiles.provenance,
		       profiles.profile_snapshot, profiles.is_public, profiles.created_at, profiles.updated_at
		FROM fused_workspace_connection_profiles profiles
		JOIN requested ON requested.service_id = profiles.service_id
		 AND requested.service_version_id = profiles.service_version_id
		 AND requested.auth_type = profiles.auth_type
		ORDER BY profiles.service_id, profiles.service_version_id, profiles.auth_type, profiles.layer DESC
	`, serviceIDs, versionIDs, authTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]WorkspaceConnectionProfile, 0, len(refs))
	for rows.Next() {
		profile, err := scanWorkspaceProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, *profile)
	}
	return profiles, rows.Err()
}

// ListWorkspaceConnectProfiles returns the effective profile for every active
// service version in one SQL query. Profiles are service/version/auth policy,
// not bucket material, so this read intentionally does not depend on a
// application credential row existing for any bucket.
func (s *postgresStore) ListWorkspaceConnectProfiles(ctx context.Context) ([]WorkspaceConnectionProfile, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ON (profiles.service_id, profiles.service_version_id, profiles.auth_type)
		       profiles.id, profiles.service_id,
		       profiles.service_version_id, profiles.auth_type, profiles.layer, profiles.registry_profile_id,
		       profiles.profile_revision, profiles.profile_hash, profiles.provenance,
		       profiles.profile_snapshot, profiles.is_public, profiles.created_at, profiles.updated_at
		FROM fused_workspace_connection_profiles profiles
		JOIN fused_workspace_service_versions versions
		  ON versions.service_id = profiles.service_id
		 AND versions.service_version_id = profiles.service_version_id
		ORDER BY profiles.service_id, profiles.service_version_id, profiles.auth_type, profiles.layer DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]WorkspaceConnectionProfile, 0)
	// SQL has already selected only exportable, effective rows; this loop
	// solely maps the bounded result into domain values for GraphQL projection.
	for rows.Next() {
		profile, err := scanWorkspaceProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, *profile)
	}
	return profiles, rows.Err()
}

// workspaceProfileRefArrays creates aligned arrays consumed by PostgreSQL
// unnest; positional alignment preserves each exact composite identity.
func workspaceProfileRefArrays(refs []WorkspaceProfileRef) ([]uuid.UUID, []uuid.UUID, []string) {
	serviceIDs := make([]uuid.UUID, 0, len(refs))
	versionIDs := make([]uuid.UUID, 0, len(refs))
	authTypes := make([]string, 0, len(refs))
	for _, ref := range refs {
		serviceIDs = append(serviceIDs, ref.ServiceID)
		versionIDs = append(versionIDs, ref.ServiceVersionID)
		authTypes = append(authTypes, ref.AuthType)
	}
	return serviceIDs, versionIDs, authTypes
}

// scanWorkspaceProfile centralizes positional mapping shared by single and
// batch profile reads.
func scanWorkspaceProfile(row interface{ Scan(...any) error }) (*WorkspaceConnectionProfile, error) {
	var profile WorkspaceConnectionProfile
	err := row.Scan(&profile.ID, &profile.ServiceID,
		&profile.ServiceVersionID, &profile.AuthType, &profile.Layer, &profile.RegistryProfileID,
		&profile.ProfileRevision, &profile.ProfileHash, &profile.Provenance,
		&profile.ProfileSnapshot, &profile.IsPublic, &profile.CreatedAt, &profile.UpdatedAt)
	return &profile, err
}

// MarkWorkspaceProfilePublished sets is_public on the effective row (override
// wins over baseline, same precedence as GetEffectiveWorkspaceProfile) after a
// successful Registry publish. Targets exactly one row via a subquery so a
// service/version/auth tuple with both layers never has both flagged.
func (s *postgresStore) MarkWorkspaceProfilePublished(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) error {
	_, err := s.db.Exec(ctx, `
		UPDATE fused_workspace_connection_profiles
		SET is_public = true, updated_at = NOW()
		WHERE id = (
			SELECT id FROM fused_workspace_connection_profiles
			WHERE service_id = $1 AND service_version_id = $2 AND auth_type = $3
			ORDER BY layer DESC
			LIMIT 1
		)`, serviceID, serviceVersionID, authType)
	return err
}

// ListWorkspaceBindingsForExecution pushes service-version and operation
// matching into SQL, resolving the effective profile's bindings (override
// layer wins over baseline) with one targeted query. bucketID is not part of
// the profile identity (Engine is mono-workspace, so there is no separate
// workspace dimension left to enforce through it) -- it is still required so
// dispatch fails closed (zero rows, via the CROSS JOIN below) if the caller
// somehow holds a bucketID that doesn't exist, rather than silently ignoring it.
func (s *postgresStore) ListWorkspaceBindingsForExecution(ctx context.Context, bucketID, serviceID, serviceVersionID uuid.UUID, authType, operationID string) ([]WorkspaceConnectionBinding, error) {
	query := `
		WITH scoped_bucket AS (
			SELECT id FROM fused_buckets WHERE id = $1
		), effective_profile AS (
			SELECT DISTINCT ON (profiles.service_id, profiles.service_version_id, profiles.auth_type) profiles.id
			FROM fused_workspace_connection_profiles profiles
			CROSS JOIN scoped_bucket
			WHERE profiles.service_id = $2 AND profiles.service_version_id = $3 AND profiles.auth_type = $4
			ORDER BY profiles.service_id, profiles.service_version_id, profiles.auth_type, profiles.layer DESC
		)
		SELECT bindings.id, bindings.service_id, bindings.service_version_id,
		       bindings.profile_id, bindings.source_kind, bindings.literal_value, bindings.source_path,
		       bindings.target_location, COALESCE(bindings.target_name, ''), bindings.operation_ids, bindings.mode,
		       bindings.provenance, bindings.source_profile_revision, bindings.created_at, bindings.updated_at
		FROM fused_workspace_connection_bindings bindings
		JOIN effective_profile ON effective_profile.id = bindings.profile_id
		WHERE cardinality(bindings.operation_ids) = 0 OR $5 = ANY(bindings.operation_ids)
		ORDER BY CASE bindings.mode WHEN 'default' THEN 0 ELSE 1 END,
		         bindings.target_location, bindings.target_name, bindings.id`
	return s.collectWorkspaceBindings(ctx, query, bucketID, serviceID, serviceVersionID, authType, operationID)
}

// ListWorkspaceProfileBindings returns the effective profile's compiled rows
// for admin/read views, leaving operation filtering to the execution-specific query.
func (s *postgresStore) ListWorkspaceProfileBindings(ctx context.Context, serviceID, serviceVersionID uuid.UUID, authType string) ([]WorkspaceConnectionBinding, error) {
	query := `
		WITH effective_profile AS (
			SELECT id FROM fused_workspace_connection_profiles
			WHERE service_id = $1 AND service_version_id = $2 AND auth_type = $3
			ORDER BY layer DESC
			LIMIT 1
		)
		SELECT bindings.id, bindings.service_id, bindings.service_version_id,
		       bindings.profile_id, bindings.source_kind, bindings.literal_value, bindings.source_path,
		       bindings.target_location, COALESCE(bindings.target_name, ''), bindings.operation_ids, bindings.mode,
		       bindings.provenance, bindings.source_profile_revision, bindings.created_at, bindings.updated_at
		FROM fused_workspace_connection_bindings bindings
		JOIN effective_profile ON effective_profile.id = bindings.profile_id
		ORDER BY CASE bindings.mode WHEN 'default' THEN 0 ELSE 1 END,
		         bindings.target_location, bindings.target_name, bindings.id`
	return s.collectWorkspaceBindings(ctx, query, serviceID, serviceVersionID, authType)
}

// collectWorkspaceBindings shares safe row iteration between already-scoped
// SQL queries without loading broader data for Go filtering.
func (s *postgresStore) collectWorkspaceBindings(ctx context.Context, query string, args ...any) ([]WorkspaceConnectionBinding, error) {
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	bindings := make([]WorkspaceConnectionBinding, 0)
	for rows.Next() {
		binding, err := scanWorkspaceBinding(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

// scanWorkspaceBinding keeps the wide runtime row mapping consistent between
// list and execution paths.
func scanWorkspaceBinding(row interface{ Scan(...any) error }) (WorkspaceConnectionBinding, error) {
	var binding WorkspaceConnectionBinding
	err := row.Scan(&binding.ID, &binding.ServiceID,
		&binding.ServiceVersionID, &binding.ProfileID, &binding.SourceKind,
		&binding.LiteralValue, &binding.SourcePath, &binding.TargetLocation,
		&binding.TargetName, &binding.OperationIDs, &binding.Mode, &binding.Provenance,
		&binding.SourceProfileRevision, &binding.CreatedAt, &binding.UpdatedAt)
	if err != nil {
		return WorkspaceConnectionBinding{}, fmt.Errorf("scan workspace binding: %w", err)
	}
	return binding, nil
}
