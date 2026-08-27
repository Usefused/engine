package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestUpsertSecretSQLDerivesWorkspaceFromBucket proves the insert still scopes
// through an owned bucket row (join against fused_buckets) rather than
// trusting a caller-supplied identifier directly -- under the mono-workspace
// model there is no per-row workspace_id column left to derive, so the
// ownership check is the bucket join itself.
func TestUpsertSecretSQLDerivesWorkspaceFromBucket(t *testing.T) {
	if !strings.Contains(upsertSecretSQL, "SELECT b.id, $2, $3, $4, $5, $6, $7") || !strings.Contains(upsertSecretSQL, "FROM fused_buckets b") {
		t.Fatalf("secret insert must derive bucket ownership from a join, not a caller-provided value: %s", upsertSecretSQL)
	}
	if strings.Contains(upsertSecretSQL, "VALUES ($1, $2") {
		t.Fatalf("secret insert must not accept caller-provided bucket ownership directly: %s", upsertSecretSQL)
	}
}

func TestFirstCompleteSecretSetSQLIsOrderedAndExpiryAware(t *testing.T) {
	for _, required := range []string{"WITH ORDINALITY", "ORDER BY ordinality", "LIMIT 1", "expires_at IS NULL OR secret.expires_at > NOW()", "value->'optional'"} {
		if !strings.Contains(firstCompleteSecretSetSQL, required) {
			t.Fatalf("ordered secret selector is missing %q: %s", required, firstCompleteSecretSetSQL)
		}
	}
	if strings.Count(firstCompleteSecretSetSQL, "expires_at IS NULL OR secret.expires_at > NOW()") != 2 {
		t.Fatal("required selection and returned optional values must both exclude expired rows")
	}
}

// TestAppBucketCredentialPresenceSQLUsesExactSecretFreeRequirements keeps
// referenced readiness set-based, expiry-aware, and free of credential values.
func TestAppBucketCredentialPresenceSQLUsesExactSecretFreeRequirements(t *testing.T) {
	for _, required := range []string{
		"jsonb_to_recordset", "config.bucket_id = $1", "config.service_id = requirement.service_id",
		"candidate.target_auth_name = requested_key.auth_name", "secret.key_name = requested_key.storage_key_name",
		"requested_key.target_key_name", "secret.expires_at IS NULL OR secret.expires_at > NOW()",
	} {
		// Each fragment preserves exact, expiry-aware matching inside one bounded query.
		if !strings.Contains(appBucketCredentialPresenceSQL, required) {
			t.Fatalf("credential readiness query is missing %q: %s", required, appBucketCredentialPresenceSQL)
		}
	}
	for _, forbidden := range []string{"encrypted_value", "encrypted_dek", "encrypted_client_id", "encrypted_client_secret"} {
		// Planning must reveal presence only, never stored credential material.
		if strings.Contains(strings.ToLower(appBucketCredentialPresenceSQL), forbidden) {
			t.Fatalf("credential readiness query must not select secret material %q", forbidden)
		}
	}
}

// TestAuthReferenceSQLUsesExactIdentityAndAliasesKeys guards the shared runtime
// invariant without permitting version drift or source names into injection.
func TestAuthReferenceSQLUsesExactIdentityAndAliasesKeys(t *testing.T) {
	for _, required := range []string{
		"candidate.target_auth_name = requested_key.auth_name",
		"candidate.value->'auth_types'->>requested_key.key_name AS auth_type",
		"requested_key.auth_type IS NULL",
		"reference.target_auth_type), '-', '_')) <> requested_key.auth_type",
		"reference.source_auth_name || SUBSTRING",
		"selected_key.target_key_name AS key_name",
		"secret.service_id = selected_key.storage_service_id",
	} {
		// Exact-name lookup and destination aliases are both required for safe reuse.
		if !strings.Contains(firstCompleteSecretSetSQL, required) {
			t.Fatalf("referenced secret selector is missing %q: %s", required, firstCompleteSecretSetSQL)
		}
	}
}

// TestPostgresStoreResolvesWholeAuthReference proves direct and referenced
// Basic bundles share one ordered, atomic store lookup and one readiness query.
func TestPostgresStoreResolvesWholeAuthReference(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	postgres := fixture.store.(*postgresStore)
	sourceServiceID := uuid.New()
	_, err := postgres.db.Exec(fixture.ctx, `
		INSERT INTO fused_workspace_services (service_id, service_name)
		VALUES ($1, 'Auth reference target'), ($2, 'Auth reference source')
	`, fixture.serviceID, sourceServiceID)
	// References may only target workspace-admitted source identities.
	if err != nil {
		t.Fatalf("seed auth reference source service: %v", err)
	}

	bindings := referencedBasicBindings(fixture.bucketA, fixture.serviceID, sourceServiceID)
	// The binding store persists direct source values and both references atomically.
	if err := fixture.store.(WorkspaceAuthBindingStore).ApplyWorkspaceAuthBindings(fixture.ctx, bindings); err != nil {
		t.Fatalf("apply auth reference fixtures: %v", err)
	}
	// A later direct row must not shadow the reference selected for this exact auth name.
	if err := fixture.store.UpsertSecrets(fixture.ctx, basicSecretRows(fixture.bucketA, fixture.serviceID, "targetBasic", "stale")); err != nil {
		t.Fatalf("seed stale target credentials: %v", err)
	}

	mismatchedAlternative := SecretKeyAlternative{
		Required:  []string{"targetBasic_username", "targetBasic_password"},
		AuthNames: map[string]string{"targetBasic_username": "targetBasic", "targetBasic_password": "targetBasic"},
		AuthTypes: map[string]string{"targetBasic_username": "bearer", "targetBasic_password": "bearer"},
	}
	selected, err := fixture.store.GetFirstCompleteSecretSet(fixture.ctx, fixture.bucketA, fixture.serviceID, []SecretKeyAlternative{mismatchedAlternative})
	// An immutable-version family change must fail closed even when source material exists.
	if err != nil || len(selected) != 0 {
		t.Fatalf("mismatched referenced Basic bundle = %#v err=%v", selected, err)
	}

	alternative := mismatchedAlternative
	alternative.AuthTypes = map[string]string{"targetBasic_username": "basic", "targetBasic_password": "basic"}
	selected, err = fixture.store.GetFirstCompleteSecretSet(fixture.ctx, fixture.bucketA, fixture.serviceID, []SecretKeyAlternative{alternative})
	// Runtime should resolve the complete source bundle in one store call.
	if err != nil {
		t.Fatalf("resolve referenced Basic bundle: %v", err)
	}
	assertReferencedBasicSelection(t, selected, sourceServiceID, "source")

	// References remain live: rotating only the source must affect the next uncached lookup.
	if err := fixture.store.UpsertSecrets(fixture.ctx, basicSecretRows(fixture.bucketA, sourceServiceID, "sourceBasic", "rotated")); err != nil {
		t.Fatalf("rotate source Basic bundle: %v", err)
	}
	selected, err = fixture.store.GetFirstCompleteSecretSet(fixture.ctx, fixture.bucketA, fixture.serviceID, []SecretKeyAlternative{alternative})
	// Rotation still has to return one complete bundle under destination keys.
	if err != nil {
		t.Fatalf("resolve rotated Basic bundle: %v", err)
	}
	assertReferencedBasicSelection(t, selected, sourceServiceID, "rotated")

	presence, err := fixture.store.(AppBucketReadinessStore).GetAppBucketCredentialPresence(fixture.ctx, fixture.bucketA, []AppCredentialRequirement{{
		ServiceID: fixture.serviceID, AuthType: "basic", AuthName: "targetBasic",
		SecretKeys: []string{"targetBasic_username", "targetBasic_password"},
	}})
	// Planning must report destination key identities even though material lives on the source.
	if err != nil || len(presence) != 1 || len(presence[0].SecretKeys) != 2 {
		t.Fatalf("referenced credential readiness = %#v err=%v", presence, err)
	}
}

// referencedBasicBindings creates overlapping target names so tests prove
// exact scheme identity wins over tempting prefix matches.
func referencedBasicBindings(bucketID, targetServiceID, sourceServiceID uuid.UUID) []WorkspaceAuthBinding {
	source := workspaceAuthDirectTestBinding(bucketID, sourceServiceID)
	return []WorkspaceAuthBinding{
		source,
		{
			BucketID: bucketID, TargetServiceID: sourceServiceID, TargetAuthType: "basic", TargetAuthName: "wrongBasic",
			TargetKeys: []string{"wrongBasic_username", "wrongBasic_password"},
			Secrets:    basicSecretRows(bucketID, sourceServiceID, "wrongBasic", "wrong"),
		},
		{
			BucketID: bucketID, TargetServiceID: targetServiceID, TargetAuthType: "basic", TargetAuthName: "target",
			TargetKeys: []string{"target_username", "target_password"},
			Reference: &WorkspaceAuthReference{
				SourceServiceID: sourceServiceID, SourceAuthType: "basic", SourceAuthName: "wrongBasic",
				SourceRequired: []string{"wrongBasic_username", "wrongBasic_password"},
			},
		},
		workspaceAuthReferenceTestBinding(bucketID, targetServiceID, sourceServiceID),
	}
}

// basicSecretRows builds one complete encrypted-metadata fixture without
// duplicating paired username/password setup across reference scenarios.
func basicSecretRows(bucketID, serviceID uuid.UUID, authName, marker string) []WorkspaceSecret {
	rows := make([]WorkspaceSecret, 0, 2)
	for _, suffix := range []string{"_username", "_password"} {
		rows = append(rows, WorkspaceSecret{
			WorkspaceSecretMeta: WorkspaceSecretMeta{
				BucketID: bucketID, ServiceID: serviceID, KeyName: authName + suffix, CredentialType: "basic",
			},
			EncryptedDEK: "dek-" + marker + suffix, EncryptedValue: "value-" + marker + suffix,
		})
	}
	return rows
}

// assertReferencedBasicSelection checks that source material is returned under
// destination names and that neither the overlapping nor stale rows won.
func assertReferencedBasicSelection(t *testing.T, selected []WorkspaceSecret, sourceServiceID uuid.UUID, marker string) {
	t.Helper()
	// A Basic reference is indivisible and must return both destination roles.
	if len(selected) != 2 {
		t.Fatalf("referenced Basic selection = %#v", selected)
	}
	byKey := make(map[string]WorkspaceSecret, len(selected))
	for _, secret := range selected {
		byKey[secret.KeyName] = secret
	}
	for _, key := range []string{"targetBasic_username", "targetBasic_password"} {
		secret, ok := byKey[key]
		// Aliased rows retain their source identity and correct encrypted marker.
		if !ok || secret.ServiceID != sourceServiceID || !strings.Contains(secret.EncryptedValue, marker) {
			t.Fatalf("referenced Basic key %q = %#v", key, secret)
		}
	}
}

func TestPostgresStore_WorkspaceSecrets(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	accountID := uuid.New()
	workspaceID := uuid.New()
	serviceID := uuid.New()
	bucketID := uuid.New()
	versionA, versionB := uuid.New(), uuid.New()

	// Setup Workspace and Bucket
	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("clean workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_app_family_buckets"); err != nil {
		t.Fatalf("clean app bucket bindings: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM fused_buckets"); err != nil {
		t.Fatalf("clean buckets: %v", err)
	}
	_, err = pool.Exec(ctx, "INSERT INTO fused_workspaces (id, account_id, name, slug) VALUES ($1, $2, $3, $4)", workspaceID, accountID, "Test WS", "test-ws")
	if err != nil {
		t.Fatalf("setup workspace failed: %v", err)
	}

	_, err = pool.Exec(ctx, "INSERT INTO fused_buckets (id, name) VALUES ($1, $2)", bucketID, "secrets-test-bucket-"+uuid.NewString())
	if err != nil {
		t.Fatalf("setup bucket failed: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO fused_workspace_services (service_id, service_name) VALUES ($1, 'Test service')`, serviceID); err != nil {
		t.Fatalf("setup service failed: %v", err)
	}
	if _, err = pool.Exec(ctx, `
		INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version)
		VALUES ($1, $2, '2026-07-08'), ($1, $3, '2026-08-01')
	`, serviceID, versionA, versionB); err != nil {
		t.Fatalf("setup workspace versions failed: %v", err)
	}

	// 1. Bucket secret
	sec1 := WorkspaceSecret{
		WorkspaceSecretMeta: WorkspaceSecretMeta{
			// A supplied value must never influence persistence; bucket ownership
			// is the single source of truth for this singleton Engine.
			BucketID:       bucketID,
			ServiceID:      serviceID,
			KeyName:        "API_KEY",
			CredentialType: "string",
		},
		EncryptedDEK:   "enc-dek",
		EncryptedValue: "enc-val",
	}
	if err := s.UpsertSecret(ctx, sec1); err != nil {
		t.Fatalf("UpsertSecret failed: %v", err)
	}
	metas, err := s.ListSecretMeta(ctx, bucketID)
	if err != nil || len(metas) != 1 || metas[0].BucketID != bucketID {
		t.Fatalf("secret workspace ownership = %#v, err=%v", metas, err)
	}

	sec2 := sec1
	sec2.KeyName = "API_SECRET"
	sec2.EncryptedValue = "enc-val-override"
	if err := s.UpsertSecret(ctx, sec2); err != nil {
		t.Fatalf("UpsertSecret 2 failed: %v", err)
	}

	// The ordered selector must fall through an expired required branch and
	// return no expired optional value from the selected branch.
	expiredAt := time.Now().UTC().Add(-time.Minute)
	expiredRequired := sec1
	expiredRequired.KeyName = "expired_required"
	expiredRequired.ExpiresAt = &expiredAt
	expiredOptional := sec1
	expiredOptional.KeyName = "expired_optional"
	expiredOptional.ExpiresAt = &expiredAt
	if err := s.UpsertSecrets(ctx, []WorkspaceSecret{expiredRequired, expiredOptional}); err != nil {
		t.Fatalf("UpsertSecrets expired selector fixtures: %v", err)
	}
	selected, err := s.GetFirstCompleteSecretSet(ctx, bucketID, serviceID, []SecretKeyAlternative{
		{Required: []string{"expired_required"}},
		{Required: []string{"API_SECRET"}, Optional: []string{"expired_optional"}},
	})
	if err != nil || len(selected) != 1 || selected[0].KeyName != "API_SECRET" {
		t.Fatalf("ordered active secret selection=%#v err=%v", selected, err)
	}
	if err := s.DeleteSecrets(ctx, bucketID, serviceID, []string{"expired_required", "expired_optional"}); err != nil {
		t.Fatalf("delete selector fixtures: %v", err)
	}

	// 3. List secrets
	metas, err = s.ListSecretMeta(ctx, bucketID)
	if err != nil || len(metas) != 2 {
		t.Fatalf("ListSecretMeta failed: %v, len=%d", err, len(metas))
	}

	secrets, err := s.ListSecretsForBucket(ctx, bucketID, serviceID)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket failed: %v, len=%d", err, len(secrets))
	}

	// 4. Update existing secret (Upsert)
	sec1.EncryptedValue = "enc-val-updated"
	if err := s.UpsertSecret(ctx, sec1); err != nil {
		t.Fatalf("UpsertSecret update failed: %v", err)
	}

	secrets, err = s.ListSecretsForBucket(ctx, bucketID, serviceID)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket after update failed: %v", err)
	}

	// Paired credentials are one UI/admin unit. Listing and deleting the pair
	// together prevents operators from leaving Basic auth in a partial state.
	basicUsername := sec1
	basicUsername.KeyName = "basicAuth_username"
	basicUsername.CredentialType = "basic"
	basicPassword := basicUsername
	basicPassword.KeyName = "basicAuth_password"
	if err := s.UpsertSecrets(ctx, []WorkspaceSecret{basicUsername, basicPassword}); err != nil {
		t.Fatalf("UpsertSecrets basic pair failed: %v", err)
	}
	page, total, err := s.ListSecretMetaPage(ctx, bucketID, 10, 0)
	if err != nil {
		t.Fatalf("ListSecretMetaPage failed: %v", err)
	}
	if total != 3 {
		t.Fatalf("ListSecretMetaPage total = %d, want two single secrets and one Basic family", total)
	}
	assertCredentialFamily(t, page, "basicAuth", []string{"basicAuth_password", "basicAuth_username"})
	if err := s.DeleteSecrets(ctx, bucketID, serviceID, []string{"basicAuth_username", "basicAuth_password"}); err != nil {
		t.Fatalf("DeleteSecrets basic pair failed: %v", err)
	}
	secrets, err = s.ListSecretsForBucket(ctx, bucketID, serviceID)
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket after family delete: %v, len=%d", err, len(secrets))
	}

	// 5. Delete secret
	if err := s.DeleteSecret(ctx, bucketID, serviceID, "API_KEY"); err != nil {
		t.Fatalf("DeleteSecret failed: %v", err)
	}

	secrets, err = s.ListSecretsForBucket(ctx, bucketID, serviceID)
	if err != nil || len(secrets) != 1 {
		t.Fatalf("ListSecretsForBucket after delete expected 1, got %d", len(secrets))
	}

	// Bucket summaries are used by the UI to show contents without issuing one
	// query per bucket, so assert the aggregate counts come from the store.
	if err := s.UpsertBucketValue(ctx, BucketValue{
		BucketID:  bucketID,
		ServiceID: serviceID,
		KeyName:   "X-Region",
		Location:  "header",
		Value:     "eu",
	}); err != nil {
		t.Fatalf("UpsertBucketValue failed: %v", err)
	}
	// Bucket values are independent of connection profiles/bindings under the
	// workspace-scoped model (plans/workspace_connection_profile_scope_plan.md):
	// they no longer promote into a compiled binding row, so ListBucketValues
	// is the only place this value should be visible.
	values, err := s.ListBucketValues(ctx, bucketID)
	if err != nil || len(values) != 1 || values[0].KeyName != "X-Region" {
		t.Fatalf("ListBucketValues after upsert = %#v, err=%v", values, err)
	}
	summaries, total, err := s.ListBucketSummaries(ctx, 10, 0)
	if err != nil {
		t.Fatalf("ListBucketSummaries failed: %v", err)
	}
	if total != 1 {
		t.Fatalf("ListBucketSummaries total = %d, want 1", total)
	}
	if len(summaries) != 1 || summaries[0].SecretCount != 1 || summaries[0].ValueCount != 1 {
		t.Fatalf("ListBucketSummaries counts = %#v, want one secret and one value", summaries)
	}
}

func assertCredentialFamily(t *testing.T, metas []WorkspaceSecretMeta, keyName string, keyNames []string) {
	t.Helper()
	for _, meta := range metas {
		if meta.KeyName == keyName {
			if len(meta.KeyNames) != len(keyNames) {
				t.Fatalf("credential family keys = %#v, want %#v", meta.KeyNames, keyNames)
			}
			for index := range keyNames {
				if meta.KeyNames[index] != keyNames[index] {
					t.Fatalf("credential family keys = %#v, want %#v", meta.KeyNames, keyNames)
				}
			}
			return
		}
	}
	t.Fatalf("credential family %q not found in %#v", keyName, metas)
}
