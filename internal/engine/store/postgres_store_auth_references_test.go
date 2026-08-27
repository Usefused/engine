package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestEncodeWorkspaceAuthBindingsKeepsValidationSecretFree proves preflight
// receives storage identities but never encrypted credential material.
func TestEncodeWorkspaceAuthBindingsKeepsValidationSecretFree(t *testing.T) {
	bucketID := uuid.New()
	serviceID := uuid.New()
	payload, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{{
		BucketID: bucketID, TargetServiceID: serviceID,
		TargetAuthType: "basic", TargetAuthName: "basicAuth",
		TargetKeys: []string{"basicAuth_username", "basicAuth_password"},
		Secrets: []WorkspaceSecret{{
			WorkspaceSecretMeta: WorkspaceSecretMeta{
				BucketID: bucketID, ServiceID: serviceID,
				KeyName: "basicAuth_username", CredentialType: "basic",
			},
			EncryptedDEK: "encrypted-dek-must-not-leak", EncryptedValue: "encrypted-value-must-not-leak",
		}},
	}})
	// A valid direct family should encode without asking SQL to inspect values.
	if err != nil {
		t.Fatalf("encode auth binding: %v", err)
	}
	encoded := string(payload)
	for _, expected := range []string{bucketID.String(), serviceID.String(), "basicAuth_username", `"direct_keys"`} {
		// Identity fields are required for exact set-based availability checks.
		if !strings.Contains(encoded, expected) {
			t.Fatalf("validation payload missing %q: %s", expected, encoded)
		}
	}
	for _, forbidden := range []string{"encrypted-dek-must-not-leak", "encrypted-value-must-not-leak", "encrypted_dek", "encrypted_value"} {
		// Encrypted blobs still reveal credential size and do not belong in preflight.
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("validation payload leaked %q: %s", forbidden, encoded)
		}
	}
}

// TestEncodeWorkspaceAuthBindingsRejectsMixedMaterial prevents one target from
// retaining copied secrets while also following a live reference.
func TestEncodeWorkspaceAuthBindingsRejectsMixedMaterial(t *testing.T) {
	bucketID := uuid.New()
	targetID := uuid.New()
	sourceID := uuid.New()
	binding := workspaceAuthReferenceTestBinding(bucketID, targetID, sourceID)
	binding.Secrets = []WorkspaceSecret{{
		WorkspaceSecretMeta: WorkspaceSecretMeta{
			BucketID: bucketID, ServiceID: targetID,
			KeyName: "targetBasic_username", CredentialType: "basic",
		},
		EncryptedDEK: "encrypted-dek", EncryptedValue: "encrypted-value",
	}}
	_, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{binding})
	// Mixed ownership would make rotation behavior ambiguous, so it must fail before SQL.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("mixed direct/reference error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
}

// TestEncodeWorkspaceAuthBindingsRejectsDuplicateTargets closes the batch-level
// path where separate rows could jointly install direct and referenced auth.
func TestEncodeWorkspaceAuthBindingsRejectsDuplicateTargets(t *testing.T) {
	bucketID := uuid.New()
	targetID := uuid.New()
	direct := WorkspaceAuthBinding{
		BucketID: bucketID, TargetServiceID: targetID,
		TargetAuthType: "basic", TargetAuthName: "targetBasic",
	}
	referenced := workspaceAuthReferenceTestBinding(bucketID, targetID, uuid.New())
	_, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{direct, referenced})
	// One target family must map to exactly one apply unit in every batch.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("duplicate target error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
}

// TestEncodeWorkspaceAuthBindingsRejectsSecretIdentityDrift prevents preflight
// from crediting material to one binding while apply writes it to another row.
func TestEncodeWorkspaceAuthBindingsRejectsSecretIdentityDrift(t *testing.T) {
	binding := WorkspaceAuthBinding{
		BucketID: uuid.New(), TargetServiceID: uuid.New(),
		TargetAuthType: "basic", TargetAuthName: "basicAuth",
	}
	binding.Secrets = []WorkspaceSecret{{
		WorkspaceSecretMeta: WorkspaceSecretMeta{
			BucketID: binding.BucketID, ServiceID: uuid.New(),
			KeyName: "basicAuth_username", CredentialType: "basic",
		},
		EncryptedDEK: "encrypted-dek", EncryptedValue: "encrypted-value",
	}}
	_, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{binding})
	// Nested material must inherit the apply unit's exact bucket and service identity.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("drifted secret identity error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
}

// TestEncodeWorkspaceAuthBindingsAcceptsExplicitClear proves auth omission can
// remove only live reference edges without inventing a direct binding.
func TestEncodeWorkspaceAuthBindingsAcceptsExplicitClear(t *testing.T) {
	payload, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{{
		BucketID: uuid.New(), TargetServiceID: uuid.New(),
		ReconcileReferences: true, ClearReferences: true,
	}})
	// Explicit service scope is what makes an identity-free clear unambiguous.
	if err != nil {
		t.Fatalf("encode clear reference binding: %v", err)
	}
	encoded := string(payload)
	// The persistence query must distinguish a clear from an empty direct family.
	if !strings.Contains(encoded, `"clear_references":true`) || !strings.Contains(encoded, `"reconcile_references":true`) {
		t.Fatalf("clear reference payload = %s", encoded)
	}
}

// TestEncodeWorkspaceAuthBindingsRejectsUnsafeClear keeps direct material and
// tuple-local deletion out of the auth-omission path.
func TestEncodeWorkspaceAuthBindingsRejectsUnsafeClear(t *testing.T) {
	binding := WorkspaceAuthBinding{BucketID: uuid.New(), TargetServiceID: uuid.New(), ClearReferences: true}
	_, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{binding})
	// A clear without explicit service-wide scope could silently leave stale edges.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("unscoped clear error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
	binding.ReconcileReferences = true
	binding.TargetAuthName = "must-not-delete-secrets"
	_, err = encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{binding})
	// Auth identity on a clear would blur reference cleanup with secret replacement.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("material-bearing clear error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
}

// TestWorkspaceAuthValidationSQLIsSetBasedAndSecretFree locks the availability,
// self-reference, chaining, and completeness guards into one bounded query.
func TestWorkspaceAuthValidationSQLIsSetBasedAndSecretFree(t *testing.T) {
	for _, expected := range []string{
		"jsonb_to_recordset($1::jsonb)",
		"binding.target_service_id = ANY($2::uuid[])",
		"binding.source_service_id = ANY($2::uuid[])",
		"FROM fused_buckets bucket",
		"FROM fused_workspace_services service",
		"binding.target_service_id = binding.source_service_id",
		"proposed_source.has_reference",
		"proposed_source.target_auth_type = binding.source_auth_type",
		"FROM fused_workspace_auth_references stored_source",
		"references cannot be chained",
		"WHEN NOT binding.clear_references AND NOT binding.has_reference AND EXISTS",
		"NOT (COALESCE(binding.direct_keys, '[]'::jsonb) ? target_key.key_name)",
		"referenced source material cannot be removed",
		"jsonb_array_elements_text(COALESCE(binding.source_required",
		"secret.expires_at IS NULL OR secret.expires_at > NOW()",
		"source credential material is incomplete",
	} {
		// Each guard belongs in the single database pass, not a Go-side lookup loop.
		if !strings.Contains(validateWorkspaceAuthBindingsSQL, expected) {
			t.Fatalf("auth validation SQL missing %q", expected)
		}
	}
	for _, forbidden := range []string{"encrypted_value", "encrypted_dek"} {
		// Availability needs only row identity and expiry, never encrypted blobs.
		if strings.Contains(strings.ToLower(validateWorkspaceAuthBindingsSQL), forbidden) {
			t.Fatalf("auth validation SQL reads credential material %q", forbidden)
		}
	}
}

// TestWorkspaceAuthApplySQLUsesFixedSetBasedStatements prevents payload size
// from increasing the number of persistence round trips.
func TestWorkspaceAuthApplySQLUsesFixedSetBasedStatements(t *testing.T) {
	checks := map[string][]string{
		lockWorkspaceAuthBucketsSQL: {
			"jsonb_to_recordset($1::jsonb)", "SELECT DISTINCT bucket_id", "ORDER BY bucket_id",
		},
		deleteWorkspaceAuthTargetsSQL: {
			"jsonb_to_recordset($1::jsonb)", "DELETE FROM fused_workspace_auth_references", "DELETE FROM fused_workspace_secrets",
			"binding.reconcile_references OR reference.target_auth_name = binding.target_auth_name", "NOT binding.clear_references",
		},
		insertWorkspaceAuthSecretsSQL: {
			"jsonb_to_recordset($1::jsonb)", "INSERT INTO fused_workspace_secrets", "ON CONFLICT ON CONSTRAINT uq_workspace_secrets",
		},
		insertWorkspaceAuthReferencesSQL: {
			"jsonb_to_recordset($1::jsonb)", "INSERT INTO fused_workspace_auth_references", "WHERE has_reference",
		},
	}
	for query, expectedFragments := range checks {
		for _, expected := range expectedFragments {
			// One recordset statement handles the whole batch without per-binding queries.
			if !strings.Contains(query, expected) {
				t.Fatalf("set-based auth apply SQL missing %q: %s", expected, query)
			}
		}
	}
}

// TestWorkspaceAuthBindingValidationUsesOneQuery proves validation query count
// remains constant as the number of bindings grows.
func TestWorkspaceAuthBindingValidationUsesOneQuery(t *testing.T) {
	recorder := &workspaceAuthValidationRecorder{}
	payload := []byte(`[{"target_auth_name":"one"},{"target_auth_name":"two"}]`)
	desiredServiceIDs := []uuid.UUID{uuid.New(), uuid.New()}
	err := validateWorkspaceAuthBindings(context.Background(), recorder, payload, desiredServiceIDs)
	// A valid no-row result must not be converted into an availability error.
	if err != nil {
		t.Fatalf("validate auth bindings: %v", err)
	}
	// The complete batch must cross the query boundary exactly once.
	if recorder.calls != 1 {
		t.Fatalf("validation query calls = %d, want 1", recorder.calls)
	}
	// Passing the original JSON once avoids hidden expansion into N placeholders.
	if len(recorder.args) != 2 || string(recorder.args[0].([]byte)) != string(payload) {
		t.Fatalf("validation query args = %#v, want payload plus desired services", recorder.args)
	}
	// Desired membership evidence remains one array argument rather than N lookups.
	if got, ok := recorder.args[1].([]uuid.UUID); !ok || len(got) != len(desiredServiceIDs) {
		t.Fatalf("desired service args = %#v, want %#v", recorder.args[1], desiredServiceIDs)
	}
}

// TestCachedStoreForwardsWorkspaceAuthPreflightAndApply proves production's
// cache wrapper preserves both halves of the reference admission contract.
func TestCachedStoreForwardsWorkspaceAuthPreflightAndApply(t *testing.T) {
	delegate := &cachedWorkspaceAuthBindingDelegate{Store: nil}
	repository, ok := NewCachedStore(delegate, nil).(WorkspaceAuthBindingStore)
	// The API receives the wrapped Store, so capability promotion is mandatory.
	if !ok {
		t.Fatal("cached store does not expose workspace auth binding capability")
	}
	binding := WorkspaceAuthBinding{
		BucketID: uuid.New(), TargetServiceID: uuid.New(),
		ReconcileReferences: true, ClearReferences: true,
	}
	desiredServiceIDs := []uuid.UUID{binding.TargetServiceID}
	if err := repository.PreflightWorkspaceAuthBindings(context.Background(), []WorkspaceAuthBinding{binding}, desiredServiceIDs); err != nil {
		t.Fatalf("forward workspace auth preflight: %v", err)
	}
	if err := repository.ApplyWorkspaceAuthBindings(context.Background(), []WorkspaceAuthBinding{binding}); err != nil {
		t.Fatalf("forward workspace auth apply: %v", err)
	}
	// Forwarding must preserve the exact desired identity and one apply batch.
	if delegate.preflightCalls != 1 || delegate.applyCalls != 1 || len(delegate.desiredServiceIDs) != 1 || delegate.desiredServiceIDs[0] != binding.TargetServiceID {
		t.Fatalf("cached auth forwarding = %#v", delegate)
	}
}

// TestPostgresWorkspaceAuthReferenceValidationGuards exercises the set-based
// source, self, chaining, completeness, and atomic-apply contract in PostgreSQL.
func TestPostgresWorkspaceAuthReferenceValidationGuards(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Unit SQL contracts still run everywhere; database behavior runs when CI provides PostgreSQL.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)
	bucketID := uuid.New()
	targetID := uuid.New()
	sourceID := uuid.New()
	upstreamID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "auth-ref-test-"+uuid.NewString()); err != nil {
		t.Fatalf("insert auth reference bucket: %v", err)
	}
	preflightTargetID := uuid.New()
	preflightSourceID := uuid.New()
	preflightBindings := []WorkspaceAuthBinding{
		workspaceAuthDirectTestBinding(bucketID, preflightSourceID),
		workspaceAuthReferenceTestBinding(bucketID, preflightTargetID, preflightSourceID),
	}
	// Read-only admission must accept identities that this same desired apply
	// will add, before any membership mutation is allowed to occur.
	if err := repository.PreflightWorkspaceAuthBindings(ctx, preflightBindings, []uuid.UUID{preflightTargetID, preflightSourceID}); err != nil {
		t.Fatalf("preflight newly desired auth graph: %v", err)
	}
	// Without plan-backed desired identities, the same absent services fail closed.
	if err := repository.PreflightWorkspaceAuthBindings(ctx, preflightBindings, nil); !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) {
		t.Fatalf("preflight absent auth graph error = %v, want ErrWorkspaceAuthReferenceInvalid", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_workspace_services (service_id, service_name)
		VALUES ($1, 'target'), ($2, 'source'), ($3, 'upstream')
	`, targetID, sourceID, upstreamID); err != nil {
		t.Fatalf("insert auth reference services: %v", err)
	}

	assertWorkspaceAuthValidationError(t, ctx, repository, workspaceAuthReferenceTestBinding(bucketID, uuid.New(), sourceID), "target service is unavailable")
	assertWorkspaceAuthValidationError(t, ctx, repository, workspaceAuthReferenceTestBinding(bucketID, targetID, uuid.New()), "source service is unavailable")
	self := workspaceAuthReferenceTestBinding(bucketID, targetID, targetID)
	self.Reference.SourceAuthName = self.TargetAuthName
	self.Reference.SourceRequired = []string{"targetBasic_username", "targetBasic_password"}
	assertWorkspaceAuthValidationError(t, ctx, repository, self, "cannot reference itself")
	assertWorkspaceAuthValidationError(t, ctx, repository, workspaceAuthReferenceTestBinding(bucketID, targetID, sourceID), "material is incomplete")

	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_workspace_auth_references (
			bucket_id, target_service_id, target_auth_type, target_auth_name,
			source_service_id, source_auth_type, source_auth_name
		) VALUES ($1, $2, 'basic', 'sourceBasic', $3, 'basic', 'upstreamBasic')
	`, bucketID, sourceID, upstreamID); err != nil {
		t.Fatalf("insert stored auth reference: %v", err)
	}
	assertWorkspaceAuthValidationError(t, ctx, repository, workspaceAuthReferenceTestBinding(bucketID, targetID, sourceID), "cannot be chained")
	if _, err := pool.Exec(ctx, `DELETE FROM fused_workspace_auth_references WHERE bucket_id = $1`, bucketID); err != nil {
		t.Fatalf("remove stored auth reference: %v", err)
	}

	sourceBinding := workspaceAuthDirectTestBinding(bucketID, sourceID)
	targetBinding := workspaceAuthReferenceTestBinding(bucketID, targetID, sourceID)
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{sourceBinding, targetBinding}); err != nil {
		t.Fatalf("apply direct source and referenced target: %v", err)
	}
	var referenceCount, secretCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_auth_references WHERE bucket_id = $1`, bucketID).Scan(&referenceCount); err != nil {
		t.Fatalf("count applied auth references: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_secrets WHERE bucket_id = $1`, bucketID).Scan(&secretCount); err != nil {
		t.Fatalf("count applied auth secrets: %v", err)
	}
	// Atomic apply stores one live edge and only the source family's two encrypted rows.
	if referenceCount != 1 || secretCount != 2 {
		t.Fatalf("applied auth state references=%d secrets=%d, want 1 and 2", referenceCount, secretCount)
	}
	incompleteSource := sourceBinding
	incompleteSource.Secrets = incompleteSource.Secrets[:1]
	err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{incompleteSource})
	// A direct rotation must not remove material still required by a live dependant.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) || !strings.Contains(err.Error(), "referenced source material cannot be removed") {
		t.Fatalf("incomplete source rotation error = %v, want safe reference validation error", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_secrets WHERE bucket_id = $1`, bucketID).Scan(&secretCount); err != nil {
		t.Fatalf("count auth secrets after denied rotation: %v", err)
	}
	// The rejected transaction must preserve both members of the source family.
	if secretCount != 2 {
		t.Fatalf("auth secrets after denied rotation = %d, want 2", secretCount)
	}
	siblingTarget := workspaceAuthDirectTestBinding(bucketID, targetID)
	siblingTarget.TargetAuthName = "siblingBasic"
	siblingTarget.TargetKeys = []string{"siblingBasic_username", "siblingBasic_password"}
	siblingTarget.Secrets = basicSecretRows(bucketID, targetID, "siblingBasic", "sibling")
	// Tuple-local replacement must not erase a separately named live reference.
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{siblingTarget}); err != nil {
		t.Fatalf("apply sibling target auth: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_auth_references WHERE bucket_id = $1 AND target_service_id = $2`, bucketID, targetID).Scan(&referenceCount); err != nil {
		t.Fatalf("count preserved sibling auth references: %v", err)
	}
	// Default persistence scope owns only the exact destination auth tuple.
	if referenceCount != 1 {
		t.Fatalf("sibling auth references after exact apply = %d, want 1", referenceCount)
	}
	renamedTarget := workspaceAuthDirectTestBinding(bucketID, targetID)
	renamedTarget.TargetAuthName = "replacementBasic"
	renamedTarget.TargetKeys = []string{"replacementBasic_username", "replacementBasic_password"}
	renamedTarget.Secrets = basicSecretRows(bucketID, targetID, "replacementBasic", "replacement")
	renamedTarget.ReconcileReferences = true
	// Replacing the service's singular configured auth family must remove its
	// old reference, or an invisible edge would keep fencing the former source.
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{renamedTarget}); err != nil {
		t.Fatalf("replace referenced target with direct auth: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_auth_references WHERE bucket_id = $1 AND target_service_id = $2`, bucketID, targetID).Scan(&referenceCount); err != nil {
		t.Fatalf("count stale target auth references: %v", err)
	}
	// Auth-name changes are reconciliations, not additive reference history.
	if referenceCount != 0 {
		t.Fatalf("stale target auth references = %d, want 0", referenceCount)
	}
	clearTargetID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_name) VALUES ($1, 'clear target')`, clearTargetID); err != nil {
		t.Fatalf("insert clear target service: %v", err)
	}
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{workspaceAuthReferenceTestBinding(bucketID, clearTargetID, sourceID)}); err != nil {
		t.Fatalf("apply clear target reference: %v", err)
	}
	clearSibling := workspaceAuthDirectTestBinding(bucketID, clearTargetID)
	clearSibling.TargetAuthName = "preservedBasic"
	clearSibling.TargetKeys = []string{"preservedBasic_username", "preservedBasic_password"}
	clearSibling.Secrets = basicSecretRows(bucketID, clearTargetID, "preservedBasic", "preserved")
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{clearSibling}); err != nil {
		t.Fatalf("apply direct material beside clear target: %v", err)
	}
	clearBinding := WorkspaceAuthBinding{
		BucketID: bucketID, TargetServiceID: clearTargetID,
		ReconcileReferences: true, ClearReferences: true,
	}
	// Auth omission removes service reference edges but is not a credential-delete request.
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{clearBinding}); err != nil {
		t.Fatalf("clear target auth references: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_auth_references WHERE bucket_id = $1 AND target_service_id = $2`, bucketID, clearTargetID).Scan(&referenceCount); err != nil {
		t.Fatalf("count cleared auth references: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_secrets WHERE bucket_id = $1 AND service_id = $2`, bucketID, clearTargetID).Scan(&secretCount); err != nil {
		t.Fatalf("count preserved direct auth material: %v", err)
	}
	// The explicit clear owns references only, leaving the two direct rows intact.
	if referenceCount != 0 || secretCount != 2 {
		t.Fatalf("cleared auth state references=%d secrets=%d, want 0 and 2", referenceCount, secretCount)
	}
	batchTargetID := uuid.New()
	batchSourceID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO fused_workspace_services (service_id, service_name)
		VALUES ($1, 'batch target'), ($2, 'batch source')
	`, batchTargetID, batchSourceID); err != nil {
		t.Fatalf("insert batch removal services: %v", err)
	}
	if err := repository.ApplyWorkspaceAuthBindings(ctx, []WorkspaceAuthBinding{
		workspaceAuthDirectTestBinding(bucketID, batchSourceID),
		workspaceAuthReferenceTestBinding(bucketID, batchTargetID, batchSourceID),
	}); err != nil {
		t.Fatalf("apply batch removal reference: %v", err)
	}
	// Removing only the source must preserve the live destination edge.
	if err := repository.RemoveWorkspaceService(ctx, batchSourceID); !errors.Is(err, ErrWorkspaceAuthReferenceInUse) {
		t.Fatalf("remove referenced source error = %v, want ErrWorkspaceAuthReferenceInUse", err)
	}
	// Removing both identities in one statement lets the target cascade finish
	// before the source dependency is checked, without weakening single deletes.
	if err := repository.RemoveWorkspaceServices(ctx, []uuid.UUID{batchSourceID, batchTargetID}); err != nil {
		t.Fatalf("remove reference target and source together: %v", err)
	}
	var remainingServices int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM fused_workspace_services WHERE service_id = ANY($1)`, []uuid.UUID{batchSourceID, batchTargetID}).Scan(&remainingServices); err != nil {
		t.Fatalf("count batch-removed services: %v", err)
	}
	// The composite removal must leave neither membership nor reference history.
	if remainingServices != 0 {
		t.Fatalf("services remaining after batch removal = %d, want 0", remainingServices)
	}
}

// assertWorkspaceAuthValidationError keeps table cases focused on the safe
// failure reason while requiring the stable store sentinel for callers.
func assertWorkspaceAuthValidationError(t *testing.T, ctx context.Context, repository *postgresStore, binding WorkspaceAuthBinding, reason string) {
	t.Helper()
	payload, err := encodeWorkspaceAuthBindings([]WorkspaceAuthBinding{binding})
	// Shape-valid cases must reach the same set-based SQL validation used inside apply.
	if err == nil {
		err = validateWorkspaceAuthBindings(ctx, repository.db, payload, []uuid.UUID{})
	}
	// Every unsafe graph shape must preserve the stable public error identity.
	if !errors.Is(err, ErrWorkspaceAuthReferenceInvalid) || !strings.Contains(err.Error(), reason) {
		t.Fatalf("validation error = %v, want ErrWorkspaceAuthReferenceInvalid containing %q", err, reason)
	}
}

// workspaceAuthDirectTestBinding supplies two encrypted rows for a complete
// Basic source without duplicating setup across persistence tests.
func workspaceAuthDirectTestBinding(bucketID, serviceID uuid.UUID) WorkspaceAuthBinding {
	secrets := make([]WorkspaceSecret, 0, 2)
	for _, keyName := range []string{"sourceBasic_username", "sourceBasic_password"} {
		secrets = append(secrets, WorkspaceSecret{
			WorkspaceSecretMeta: WorkspaceSecretMeta{
				BucketID: bucketID, ServiceID: serviceID, KeyName: keyName, CredentialType: "basic",
			},
			EncryptedDEK: "encrypted-dek-" + keyName, EncryptedValue: "encrypted-value-" + keyName,
		})
	}
	return WorkspaceAuthBinding{
		BucketID: bucketID, TargetServiceID: serviceID,
		TargetAuthType: "basic", TargetAuthName: "sourceBasic",
		TargetKeys: []string{"sourceBasic_username", "sourceBasic_password"}, Secrets: secrets,
	}
}

// workspaceAuthReferenceTestBinding supplies a complete reference identity so
// validation-shape tests can vary only the invariant under test.
func workspaceAuthReferenceTestBinding(bucketID, targetID, sourceID uuid.UUID) WorkspaceAuthBinding {
	return WorkspaceAuthBinding{
		BucketID: bucketID, TargetServiceID: targetID,
		TargetAuthType: "basic", TargetAuthName: "targetBasic",
		TargetKeys: []string{"targetBasic_username", "targetBasic_password"},
		Reference: &WorkspaceAuthReference{
			SourceServiceID: sourceID, SourceAuthType: "basic", SourceAuthName: "sourceBasic",
			SourceRequired: []string{"sourceBasic_username", "sourceBasic_password"},
		},
	}
}

type workspaceAuthValidationRecorder struct {
	calls int
	args  []any
}

type cachedWorkspaceAuthBindingDelegate struct {
	Store
	preflightCalls    int
	applyCalls        int
	desiredServiceIDs []uuid.UUID
}

// PreflightWorkspaceAuthBindings records the read-only batch received through
// the production wrapper without adding a competing validation path.
func (d *cachedWorkspaceAuthBindingDelegate) PreflightWorkspaceAuthBindings(_ context.Context, _ []WorkspaceAuthBinding, desiredServiceIDs []uuid.UUID) error {
	d.preflightCalls++
	d.desiredServiceIDs = append([]uuid.UUID(nil), desiredServiceIDs...)
	return nil
}

// ApplyWorkspaceAuthBindings records the post-admission transactional batch
// while the cached store owns any resulting invalidation.
func (d *cachedWorkspaceAuthBindingDelegate) ApplyWorkspaceAuthBindings(_ context.Context, _ []WorkspaceAuthBinding) error {
	d.applyCalls++
	return nil
}

// QueryRow records the single batch call and returns the valid no-error shape.
func (r *workspaceAuthValidationRecorder) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	r.calls++
	r.args = args
	return workspaceAuthNoRows{}
}

type workspaceAuthNoRows struct{}

// Scan models PostgreSQL returning no invalid binding from the validation CTE.
func (workspaceAuthNoRows) Scan(...any) error {
	return pgx.ErrNoRows
}
