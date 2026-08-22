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

func TestAppBucketCredentialPresenceSQLUsesExactSecretFreeRequirements(t *testing.T) {
	for _, required := range []string{"jsonb_to_recordset", "config.bucket_id = $1", "config.service_id = requirement.service_id", "secret.key_name = requested_key.key_name"} {
		if !strings.Contains(appBucketCredentialPresenceSQL, required) {
			t.Fatalf("credential readiness query is missing %q: %s", required, appBucketCredentialPresenceSQL)
		}
	}
	for _, forbidden := range []string{"encrypted_value", "encrypted_dek", "encrypted_client_id", "encrypted_client_secret"} {
		if strings.Contains(strings.ToLower(appBucketCredentialPresenceSQL), forbidden) {
			t.Fatalf("credential readiness query must not select secret material %q", forbidden)
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
