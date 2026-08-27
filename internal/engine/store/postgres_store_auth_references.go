package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type workspaceAuthBindingPayload struct {
	BucketID            uuid.UUID `json:"bucket_id"`
	TargetServiceID     uuid.UUID `json:"target_service_id"`
	TargetAuthType      string    `json:"target_auth_type"`
	TargetAuthName      string    `json:"target_auth_name"`
	TargetKeys          []string  `json:"target_keys"`
	DirectKeys          []string  `json:"direct_keys"`
	HasReference        bool      `json:"has_reference"`
	ReconcileReferences bool      `json:"reconcile_references"`
	ClearReferences     bool      `json:"clear_references"`
	SourceServiceID     uuid.UUID `json:"source_service_id,omitempty"`
	SourceAuthType      string    `json:"source_auth_type,omitempty"`
	SourceAuthName      string    `json:"source_auth_name,omitempty"`
	SourceRequired      []string  `json:"source_required,omitempty"`
}

type workspaceAuthSecretPayload struct {
	BucketID       uuid.UUID  `json:"bucket_id"`
	ServiceID      uuid.UUID  `json:"service_id"`
	KeyName        string     `json:"key_name"`
	CredentialType string     `json:"credential_type"`
	EncryptedDEK   string     `json:"encrypted_dek"`
	EncryptedValue string     `json:"encrypted_value"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
}

const validateWorkspaceAuthBindingsSQL = `
	WITH bindings AS (
		SELECT *
		FROM jsonb_to_recordset($1::jsonb) AS binding(
			bucket_id uuid,
			target_service_id uuid,
			target_auth_type text,
			target_auth_name text,
			target_keys jsonb,
			direct_keys jsonb,
			has_reference boolean,
			reconcile_references boolean,
			clear_references boolean,
			source_service_id uuid,
			source_auth_type text,
			source_auth_name text,
			source_required jsonb
		)
	), invalid AS (
		SELECT binding.target_service_id, binding.target_auth_name,
			CASE
				WHEN NOT EXISTS (SELECT 1 FROM fused_buckets bucket WHERE bucket.id = binding.bucket_id)
					THEN 'bucket is unavailable'
				WHEN NOT EXISTS (SELECT 1 FROM fused_workspace_services service WHERE service.service_id = binding.target_service_id)
					AND NOT (binding.target_service_id = ANY($2::uuid[]))
					THEN 'target service is unavailable'
				WHEN binding.has_reference AND NOT EXISTS (SELECT 1 FROM fused_workspace_services service WHERE service.service_id = binding.source_service_id)
					AND NOT (binding.source_service_id = ANY($2::uuid[]))
					THEN 'source service is unavailable'
				WHEN binding.has_reference AND binding.target_auth_type <> binding.source_auth_type
					THEN 'source and target auth types differ'
				WHEN binding.has_reference AND binding.target_service_id = binding.source_service_id AND binding.target_auth_name = binding.source_auth_name
					THEN 'a credential cannot reference itself'
				WHEN binding.has_reference AND EXISTS (
					SELECT 1 FROM bindings proposed_source
					WHERE proposed_source.bucket_id = binding.bucket_id
					  AND proposed_source.target_service_id = binding.source_service_id
					  AND proposed_source.target_auth_name = binding.source_auth_name
					  AND proposed_source.target_auth_type = binding.source_auth_type
					  AND proposed_source.has_reference
				) THEN 'references cannot be chained'
				WHEN binding.has_reference AND EXISTS (
					SELECT 1 FROM fused_workspace_auth_references dependent
					WHERE dependent.bucket_id = binding.bucket_id
					  AND dependent.source_service_id = binding.target_service_id
					  AND dependent.source_auth_name = binding.target_auth_name
					  AND NOT EXISTS (
						SELECT 1 FROM bindings replacement
						WHERE replacement.bucket_id = dependent.bucket_id
						  AND replacement.target_service_id = dependent.target_service_id
						  AND replacement.target_auth_name = dependent.target_auth_name
					  )
				) THEN 'a referenced source cannot itself become a reference'
				WHEN NOT binding.clear_references AND NOT binding.has_reference AND EXISTS (
					SELECT 1
					FROM fused_workspace_auth_references dependent,
					     jsonb_array_elements_text(COALESCE(binding.target_keys, '[]'::jsonb)) target_key(key_name)
					WHERE dependent.bucket_id = binding.bucket_id
					  AND dependent.source_service_id = binding.target_service_id
					  AND dependent.source_auth_type = binding.target_auth_type
					  AND dependent.source_auth_name = binding.target_auth_name
					  AND NOT (COALESCE(binding.direct_keys, '[]'::jsonb) ? target_key.key_name)
					  AND EXISTS (
						SELECT 1 FROM fused_workspace_secrets secret
						WHERE secret.bucket_id = binding.bucket_id
						  AND secret.service_id = binding.target_service_id
						  AND secret.key_name = target_key.key_name
					  )
				) THEN 'referenced source material cannot be removed'
				WHEN binding.has_reference AND NOT EXISTS (
					SELECT 1 FROM bindings proposed_source
					WHERE proposed_source.bucket_id = binding.bucket_id
					  AND proposed_source.target_service_id = binding.source_service_id
					  AND proposed_source.target_auth_name = binding.source_auth_name
					  AND proposed_source.target_auth_type = binding.source_auth_type
					  AND NOT proposed_source.has_reference
				) AND EXISTS (
					SELECT 1 FROM fused_workspace_auth_references stored_source
					WHERE stored_source.bucket_id = binding.bucket_id
					  AND stored_source.target_service_id = binding.source_service_id
					  AND stored_source.target_auth_name = binding.source_auth_name
					  AND stored_source.target_auth_type = binding.source_auth_type
				) THEN 'references cannot be chained'
				WHEN binding.has_reference AND EXISTS (
					SELECT 1
					FROM jsonb_array_elements_text(COALESCE(binding.source_required, '[]'::jsonb)) required_key(key_name)
					WHERE CASE
						WHEN EXISTS (
							SELECT 1 FROM bindings proposed_source
							WHERE proposed_source.bucket_id = binding.bucket_id
							  AND proposed_source.target_service_id = binding.source_service_id
							  AND proposed_source.target_auth_name = binding.source_auth_name
							  AND proposed_source.target_auth_type = binding.source_auth_type
							  AND NOT proposed_source.has_reference
						) THEN NOT EXISTS (
							SELECT 1 FROM bindings proposed_source,
								jsonb_array_elements_text(COALESCE(proposed_source.direct_keys, '[]'::jsonb)) direct_key(key_name)
							WHERE proposed_source.bucket_id = binding.bucket_id
							  AND proposed_source.target_service_id = binding.source_service_id
							  AND proposed_source.target_auth_name = binding.source_auth_name
							  AND proposed_source.target_auth_type = binding.source_auth_type
							  AND NOT proposed_source.has_reference
							  AND direct_key.key_name = required_key.key_name
						) ELSE NOT EXISTS (
							SELECT 1 FROM fused_workspace_secrets secret
							WHERE secret.bucket_id = binding.bucket_id
							  AND secret.service_id = binding.source_service_id
							  AND secret.key_name = required_key.key_name
							  AND (secret.expires_at IS NULL OR secret.expires_at > NOW())
						) END
				) THEN 'source credential material is incomplete'
			END AS reason
		FROM bindings binding
	)
	SELECT reason
	FROM invalid
	WHERE reason IS NOT NULL
	ORDER BY target_service_id, target_auth_name
	LIMIT 1`

const deleteWorkspaceAuthTargetsSQL = `
	WITH bindings AS (
		SELECT bucket_id, target_service_id, target_auth_name,
		       COALESCE(target_keys, '[]'::jsonb) AS target_keys,
		       COALESCE(direct_keys, '[]'::jsonb) AS direct_keys,
		       has_reference, reconcile_references, clear_references
		FROM jsonb_to_recordset($1::jsonb) AS binding(
			bucket_id uuid, target_service_id uuid, target_auth_name text,
			target_keys jsonb, direct_keys jsonb, has_reference boolean,
			reconcile_references boolean, clear_references boolean
		)
	), deleted_refs AS (
		DELETE FROM fused_workspace_auth_references reference
		USING bindings binding
		WHERE reference.bucket_id = binding.bucket_id
		  AND reference.target_service_id = binding.target_service_id
		  AND (binding.reconcile_references OR reference.target_auth_name = binding.target_auth_name)
		RETURNING 1
	)
	DELETE FROM fused_workspace_secrets secret
	USING bindings binding, LATERAL jsonb_array_elements_text(binding.target_keys) target_key(key_name)
	WHERE secret.bucket_id = binding.bucket_id
	  AND secret.service_id = binding.target_service_id
	  AND secret.key_name = target_key.key_name
	  AND NOT binding.clear_references
	  AND (binding.has_reference OR NOT (binding.direct_keys ? target_key.key_name))`

const lockWorkspaceAuthBucketsSQL = `
	SELECT pg_advisory_xact_lock(hashtextextended(bucket_id::text, 0))
	FROM (
		SELECT DISTINCT bucket_id
		FROM jsonb_to_recordset($1::jsonb) AS binding(bucket_id uuid)
	) buckets
	ORDER BY bucket_id`

const insertWorkspaceAuthSecretsSQL = `
	INSERT INTO fused_workspace_secrets
		(bucket_id, service_id, key_name, credential_type, encrypted_dek, encrypted_value, expires_at)
	SELECT bucket.id, input.service_id, input.key_name, input.credential_type,
	       input.encrypted_dek, input.encrypted_value, input.expires_at
	FROM jsonb_to_recordset($1::jsonb) AS input(
		bucket_id uuid, service_id uuid, key_name text, credential_type text,
		encrypted_dek text, encrypted_value text, expires_at timestamptz
	)
	JOIN fused_buckets bucket ON bucket.id = input.bucket_id
	ON CONFLICT ON CONSTRAINT uq_workspace_secrets
	DO UPDATE SET credential_type = EXCLUDED.credential_type,
	              encrypted_dek = EXCLUDED.encrypted_dek,
	              encrypted_value = EXCLUDED.encrypted_value,
	              expires_at = EXCLUDED.expires_at,
	              updated_at = NOW()`

const insertWorkspaceAuthReferencesSQL = `
	INSERT INTO fused_workspace_auth_references
		(bucket_id, target_service_id, target_auth_type, target_auth_name,
		 source_service_id, source_auth_type, source_auth_name)
	SELECT bucket_id, target_service_id, target_auth_type, target_auth_name,
	       source_service_id, source_auth_type, source_auth_name
	FROM jsonb_to_recordset($1::jsonb) AS binding(
		bucket_id uuid, target_service_id uuid, target_auth_type text,
		target_auth_name text, has_reference boolean, source_service_id uuid,
		source_auth_type text, source_auth_name text
	)
	WHERE has_reference
	ON CONFLICT ON CONSTRAINT uq_fused_workspace_auth_reference_target
	DO UPDATE SET target_auth_type = EXCLUDED.target_auth_type,
	              source_service_id = EXCLUDED.source_service_id,
	              source_auth_type = EXCLUDED.source_auth_type,
	              source_auth_name = EXCLUDED.source_auth_name,
	              updated_at = NOW()`

// PreflightWorkspaceAuthBindings validates a complete desired graph without
// requiring newly planned service memberships to have been written first.
func (s *postgresStore) PreflightWorkspaceAuthBindings(ctx context.Context, bindings []WorkspaceAuthBinding, desiredServiceIDs []uuid.UUID) error {
	// An auth-free desired graph has no reference invariant to validate.
	if len(bindings) == 0 {
		return nil
	}
	// Desired identities are admission evidence only; nil identities could
	// otherwise make an unavailable service look intentionally planned.
	if err := validateDesiredWorkspaceServiceIDs(desiredServiceIDs); err != nil {
		return err
	}
	// PostgreSQL ANY(NULL) is unknown, so an absent allowance must be encoded as
	// an empty typed array to preserve fail-closed availability checks.
	if desiredServiceIDs == nil {
		desiredServiceIDs = []uuid.UUID{}
	}
	payload, err := encodeWorkspaceAuthBindings(bindings)
	// Shape validation is shared with transactional apply so preflight cannot
	// accept an operation the commit path will later interpret differently.
	if err != nil {
		return err
	}
	return validateWorkspaceAuthBindings(ctx, s.db, payload, desiredServiceIDs)
}

// ApplyWorkspaceAuthBindings replaces all declared target families in one
// transaction so dispatch never observes both copied material and a reference.
func (s *postgresStore) ApplyWorkspaceAuthBindings(ctx context.Context, bindings []WorkspaceAuthBinding) error {
	// An auth-free config has no persistence or cache-visible work.
	if len(bindings) == 0 {
		return nil
	}
	bindingPayload, secretPayload, secretCount, err := encodeWorkspaceAuthApplyPayloads(bindings)
	// Encoding owns the shared exclusivity and identity admission boundary.
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	// Reference replacement must commit with any paired direct-secret rotation.
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return applyWorkspaceAuthBindingsTx(ctx, tx, bindingPayload, secretPayload, secretCount)
}

// encodeWorkspaceAuthApplyPayloads validates the shared shape once before
// producing the secret-free binding and encrypted direct-material batches.
func encodeWorkspaceAuthApplyPayloads(bindings []WorkspaceAuthBinding) ([]byte, []byte, int, error) {
	bindingPayload, err := encodeWorkspaceAuthBindings(bindings)
	// A rejected outer binding must never produce the encrypted companion batch.
	if err != nil {
		return nil, nil, 0, err
	}
	secretPayload, secretCount, err := encodeWorkspaceAuthSecrets(bindings)
	return bindingPayload, secretPayload, secretCount, err
}

// applyWorkspaceAuthBindingsTx owns the fixed-count SQL sequence under one
// bucket-ordered transaction lock.
func applyWorkspaceAuthBindingsTx(ctx context.Context, tx pgx.Tx, bindingPayload, secretPayload []byte, secretCount int) error {
	// Bucket-ordered transaction locks serialize source deletion against
	// reference creation without adding a per-service lock query.
	if _, err := tx.Exec(ctx, lockWorkspaceAuthBucketsSQL, bindingPayload); err != nil {
		return err
	}
	// Repeating preflight under the write transaction closes the rotation race
	// between the public validation phase and persistence.
	if err := validateWorkspaceAuthBindings(ctx, tx, bindingPayload, []uuid.UUID{}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, deleteWorkspaceAuthTargetsSQL, bindingPayload); err != nil {
		return err
	}
	// Reference-only batches deliberately carry no encrypted insert payload.
	if secretCount > 0 {
		tag, err := tx.Exec(ctx, insertWorkspaceAuthSecretsSQL, secretPayload)
		if err != nil {
			return err
		}
		// A missing bucket must fail closed instead of silently dropping one
		// credential row through the ownership join.
		if tag.RowsAffected() != int64(secretCount) {
			return ErrWorkspaceAuthReferenceInvalid
		}
	}
	if _, err := tx.Exec(ctx, insertWorkspaceAuthReferencesSQL, bindingPayload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type workspaceAuthValidationQuery interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// validateWorkspaceAuthBindings converts one safe reason into a stable store
// sentinel without exposing source keys or credential material.
func validateWorkspaceAuthBindings(ctx context.Context, query workspaceAuthValidationQuery, payload []byte, desiredServiceIDs []uuid.UUID) error {
	var reason *string
	// No row is the successful set-wide outcome; other query errors remain
	// infrastructure failures rather than reference validation results.
	if err := query.QueryRow(ctx, validateWorkspaceAuthBindingsSQL, payload, desiredServiceIDs).Scan(&reason); err != nil && err != pgx.ErrNoRows {
		return err
	}
	// The query returns no row when every binding is ready; a non-empty reason
	// is safe operator context attached to the stable reference error.
	if reason != nil && *reason != "" {
		return fmt.Errorf("%w: %s", ErrWorkspaceAuthReferenceInvalid, *reason)
	}
	return nil
}

// validateDesiredWorkspaceServiceIDs keeps the preflight allowance limited to
// concrete Registry identities already admitted by the workspace plan.
func validateDesiredWorkspaceServiceIDs(serviceIDs []uuid.UUID) error {
	for _, serviceID := range serviceIDs {
		// A nil desired identity cannot correspond to an activatable service.
		if serviceID == uuid.Nil {
			return fmt.Errorf("%w: desired service identity is incomplete", ErrWorkspaceAuthReferenceInvalid)
		}
	}
	return nil
}

// encodeWorkspaceAuthBindings builds one secret-free SQL payload shared by
// public preflight and transactional revalidation.
func encodeWorkspaceAuthBindings(bindings []WorkspaceAuthBinding) ([]byte, error) {
	// Both standalone preflight and apply must enforce the same Go-side shape.
	if err := validateWorkspaceAuthBindingShapes(bindings); err != nil {
		return nil, err
	}
	payload := make([]workspaceAuthBindingPayload, 0, len(bindings))
	for _, binding := range bindings {
		item := workspaceAuthBindingPayload{
			BucketID: binding.BucketID, TargetServiceID: binding.TargetServiceID,
			TargetAuthType: binding.TargetAuthType, TargetAuthName: binding.TargetAuthName,
			TargetKeys: binding.TargetKeys, DirectKeys: workspaceAuthSecretKeys(binding.Secrets),
			ReconcileReferences: binding.ReconcileReferences, ClearReferences: binding.ClearReferences,
		}
		// Only reference bindings carry source identity; direct bindings keep
		// zero UUIDs out of validation comparisons.
		if binding.Reference != nil {
			item.HasReference = true
			item.SourceServiceID = binding.Reference.SourceServiceID
			item.SourceAuthType = binding.Reference.SourceAuthType
			item.SourceAuthName = binding.Reference.SourceAuthName
			item.SourceRequired = binding.Reference.SourceRequired
		}
		payload = append(payload, item)
	}
	return json.Marshal(payload)
}

// validateWorkspaceAuthBindingShapes enforces exclusivity and identity before
// either the validation or encrypted apply payload crosses into SQL.
func validateWorkspaceAuthBindingShapes(bindings []WorkspaceAuthBinding) error {
	seen := make(map[string]struct{}, len(bindings))
	serviceCounts := workspaceAuthBindingServiceCounts(bindings)
	for _, binding := range bindings {
		identity := binding.BucketID.String() + "\x00" + binding.TargetServiceID.String() + "\x00" + binding.TargetAuthName
		// One target family can have exactly one desired source in an apply.
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("%w: duplicate target auth binding", ErrWorkspaceAuthReferenceInvalid)
		}
		seen[identity] = struct{}{}
		serviceIdentity := binding.BucketID.String() + "\x00" + binding.TargetServiceID.String()
		// Service-wide reconciliation is an explicit singular scope; mixing it
		// with sibling bindings would make replacement intent order-dependent.
		if binding.ReconcileReferences && serviceCounts[serviceIdentity] > 1 {
			return fmt.Errorf("%w: service-wide auth reconciliation has sibling bindings", ErrWorkspaceAuthReferenceInvalid)
		}
		// A reference and encrypted rows are mutually exclusive representations
		// of the same complete credential family.
		if binding.Reference != nil && len(binding.Secrets) > 0 {
			return fmt.Errorf("%w: auth binding mixes a reference with direct material", ErrWorkspaceAuthReferenceInvalid)
		}
		if err := validateWorkspaceAuthBindingIdentity(binding); err != nil {
			return err
		}
	}
	return nil
}

// validateWorkspaceAuthBindingIdentity prevents preflight identity from
// disagreeing with where nested encrypted rows would actually be written.
func validateWorkspaceAuthBindingIdentity(binding WorkspaceAuthBinding) error {
	// Clear intent owns only the service's reference edges and deliberately
	// leaves every direct secret row untouched.
	if binding.ClearReferences {
		return validateWorkspaceAuthClearBinding(binding)
	}
	// Database identifiers and an owned key family are the minimum safe target.
	if !workspaceAuthTargetIdentityComplete(binding) {
		return fmt.Errorf("%w: target auth identity is incomplete", ErrWorkspaceAuthReferenceInvalid)
	}
	// Key-set validation prevents delete scope from escaping the selected family.
	if err := validateWorkspaceAuthBindingKeys(binding); err != nil {
		return err
	}
	// Nested rows cannot contradict the outer preflight identity.
	if err := validateWorkspaceAuthSecretIdentities(binding); err != nil {
		return err
	}
	// Direct material needs at least one row; references instead require a
	// complete, type-compatible source selector.
	// Direct and referenced bindings have different completeness contracts.
	if binding.Reference == nil {
		// Empty direct material would erase an existing credential family.
		if len(binding.Secrets) == 0 {
			return fmt.Errorf("%w: direct auth binding has no material", ErrWorkspaceAuthReferenceInvalid)
		}
		return nil
	}
	ref := binding.Reference
	// References need one exact compatible source family and required key set.
	if !workspaceAuthSourceIdentityComplete(binding.TargetAuthType, ref) {
		return fmt.Errorf("%w: source auth identity is incomplete or incompatible", ErrWorkspaceAuthReferenceInvalid)
	}
	// An exact self-edge could otherwise pass source readiness by reading the
	// target's stale direct rows before replacement.
	if ref.SourceServiceID == binding.TargetServiceID && ref.SourceAuthName == binding.TargetAuthName {
		return fmt.Errorf("%w: a credential cannot reference itself", ErrWorkspaceAuthReferenceInvalid)
	}
	return nil
}

// validateWorkspaceAuthClearBinding prevents a reference-only clear from
// silently carrying auth identity or direct-material deletion scope.
func validateWorkspaceAuthClearBinding(binding WorkspaceAuthBinding) error {
	// A clear must name one concrete bucket service and explicitly claim the
	// service-wide reference scope that it removes.
	if binding.BucketID == uuid.Nil || binding.TargetServiceID == uuid.Nil || !binding.ReconcileReferences {
		return fmt.Errorf("%w: clear reference identity is incomplete", ErrWorkspaceAuthReferenceInvalid)
	}
	// Empty auth fields and material are what guarantee direct secrets survive.
	if binding.TargetAuthType != "" || binding.TargetAuthName != "" || len(binding.TargetKeys) > 0 || len(binding.Secrets) > 0 || binding.Reference != nil {
		return fmt.Errorf("%w: clear reference binding carries auth material", ErrWorkspaceAuthReferenceInvalid)
	}
	return nil
}

// workspaceAuthBindingServiceCounts derives reconciliation cardinality in one
// pass so validation cost stays linear as composite workspace applies grow.
func workspaceAuthBindingServiceCounts(bindings []WorkspaceAuthBinding) map[string]int {
	counts := make(map[string]int, len(bindings))
	for _, binding := range bindings {
		identity := binding.BucketID.String() + "\x00" + binding.TargetServiceID.String()
		counts[identity]++
	}
	return counts
}

// workspaceAuthTargetIdentityComplete keeps compound shape checks out of the
// decision-heavy validator while retaining one canonical invariant.
func workspaceAuthTargetIdentityComplete(binding WorkspaceAuthBinding) bool {
	return binding.BucketID != uuid.Nil && binding.TargetServiceID != uuid.Nil && binding.TargetAuthType != "" && binding.TargetAuthName != "" && len(binding.TargetKeys) > 0
}

// workspaceAuthSourceIdentityComplete defines the minimum complete source
// selector and requires canonical type equality with its destination.
func workspaceAuthSourceIdentityComplete(targetAuthType string, ref *WorkspaceAuthReference) bool {
	return ref.SourceServiceID != uuid.Nil && ref.SourceAuthType != "" && ref.SourceAuthName != "" && len(ref.SourceRequired) > 0 && ref.SourceAuthType == targetAuthType
}

// validateWorkspaceAuthSecretIdentities prevents the direct rows credited by
// preflight from being persisted under another bucket or service.
func validateWorkspaceAuthSecretIdentities(binding WorkspaceAuthBinding) error {
	for _, secret := range binding.Secrets {
		// SQL preflight credits direct keys to the outer binding, so every secret
		// must share that exact bucket and service identity.
		if secret.BucketID != binding.BucketID || secret.ServiceID != binding.TargetServiceID {
			return fmt.Errorf("%w: direct material identity does not match its target", ErrWorkspaceAuthReferenceInvalid)
		}
	}
	return nil
}

// validateWorkspaceAuthBindingKeys keeps deletion and upsert sets inside the
// target family admitted by the outer binding identity.
func validateWorkspaceAuthBindingKeys(binding WorkspaceAuthBinding) error {
	targetKeys := make(map[string]struct{}, len(binding.TargetKeys))
	for _, key := range binding.TargetKeys {
		// Empty or duplicate target keys make delete semantics ambiguous.
		if key == "" {
			return fmt.Errorf("%w: target auth key is empty", ErrWorkspaceAuthReferenceInvalid)
		}
		targetKeys[key] = struct{}{}
	}
	if len(targetKeys) != len(binding.TargetKeys) {
		return fmt.Errorf("%w: target auth keys are duplicated", ErrWorkspaceAuthReferenceInvalid)
	}
	directKeys := make(map[string]struct{}, len(binding.Secrets))
	for _, secret := range binding.Secrets {
		// Direct rows must be a subset of the family keys replacement owns.
		if _, ok := targetKeys[secret.KeyName]; !ok {
			return fmt.Errorf("%w: direct material key is outside its target family", ErrWorkspaceAuthReferenceInvalid)
		}
		directKeys[secret.KeyName] = struct{}{}
	}
	if len(directKeys) != len(binding.Secrets) {
		return fmt.Errorf("%w: direct material keys are duplicated", ErrWorkspaceAuthReferenceInvalid)
	}
	return nil
}

// workspaceAuthSecretKeys projects only storage identities for preflight;
// encryption payloads never enter the validation query.
func workspaceAuthSecretKeys(secrets []WorkspaceSecret) []string {
	keys := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		keys = append(keys, secret.KeyName)
	}
	return keys
}

// encodeWorkspaceAuthSecrets flattens direct bindings for one set-based
// upsert while leaving referenced bindings value-free.
func encodeWorkspaceAuthSecrets(bindings []WorkspaceAuthBinding) ([]byte, int, error) {
	var payload []workspaceAuthSecretPayload
	for _, binding := range bindings {
		for _, secret := range binding.Secrets {
			payload = append(payload, workspaceAuthSecretPayload{
				BucketID: secret.BucketID, ServiceID: secret.ServiceID, KeyName: secret.KeyName,
				CredentialType: secret.CredentialType, EncryptedDEK: secret.EncryptedDEK,
				EncryptedValue: secret.EncryptedValue, ExpiresAt: secret.ExpiresAt,
			})
		}
	}
	encoded, err := json.Marshal(payload)
	return encoded, len(payload), err
}
