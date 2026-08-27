package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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

// TestUpsertSecretsSQLUsesOneSetStatement guards the no-N+1 family-write boundary.
func TestUpsertSecretsSQLUsesOneSetStatement(t *testing.T) {
	for _, required := range []string{"jsonb_to_recordset", "JOIN fused_buckets", "ON CONFLICT", "SELECT COUNT(*) FROM inserted"} {
		// Each fragment preserves set expansion, ownership admission, idempotence, and short-write detection.
		if !strings.Contains(upsertSecretsSQL, required) {
			t.Fatalf("bulk secret upsert is missing %q: %s", required, upsertSecretsSQL)
		}
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

// TestAppBucketCredentialPresenceSQLUsesExactSecretFreeRequirements keeps app-source readiness set-based and credential-free.
func TestAppBucketCredentialPresenceSQLUsesExactSecretFreeRequirements(t *testing.T) {
	for _, required := range []string{
		"jsonb_to_recordset", "secret.bucket_id = $1", "requested_key.storage_service_id",
		"requested_key.source_service_id", "requested_key.source_auth_name",
		"secret.key_name = requested_key.storage_key_name", "secret.expires_at IS NULL OR secret.expires_at > NOW()",
	} {
		// Each fragment preserves exact app-owned source rebasing inside one bounded query.
		if !strings.Contains(appBucketCredentialPresenceSQL, required) {
			t.Fatalf("credential readiness query is missing %q: %s", required, appBucketCredentialPresenceSQL)
		}
	}
	for _, forbidden := range []string{"fused_workspace_auth_references", "encrypted_value", "encrypted_dek", "encrypted_client_id", "encrypted_client_secret"} {
		// Planning must reveal presence only and cannot depend on retired workspace-global edges.
		if strings.Contains(strings.ToLower(appBucketCredentialPresenceSQL), forbidden) {
			t.Fatalf("credential readiness query contains forbidden material %q", forbidden)
		}
	}
}

// TestAppCredentialReferenceSQLRebasesImmutableSource verifies runtime selection uses only app-carried source identity.
func TestAppCredentialReferenceSQLRebasesImmutableSource(t *testing.T) {
	for _, required := range []string{
		"candidate.value->>'source_service_id'", "candidate.value->>'source_auth_type'",
		"candidate.value->>'source_auth_name'", "selected_key.target_key_name AS key_name",
		"secret.service_id = selected_key.storage_service_id", "00000000-0000-0000-0000-000000000000",
	} {
		// Source identity and target aliases must remain in the same set-based lookup.
		if !strings.Contains(firstCompleteSecretSetSQL, required) {
			t.Fatalf("app credential selector is missing %q: %s", required, firstCompleteSecretSetSQL)
		}
	}
	// The retired workspace graph must never reappear on the runtime path.
	if strings.Contains(firstCompleteSecretSetSQL, "fused_workspace_auth_references") {
		t.Fatal("runtime credential lookup must not query workspace auth references")
	}
}

// TestPostgresStoreResolvesOAuthApplicationReference covers same-bucket application reuse without a workspace reference row.
func TestPostgresStoreResolvesOAuthApplicationReference(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	sourceServiceID := uuid.New()
	targetKeys := []string{"sheetsOAuth_client_id", "sheetsOAuth_client_secret"}
	// The referenced lookup is meaningful only when both application credentials exist on the source service.
	if err := fixture.store.UpsertSecrets(fixture.ctx, oauthApplicationSecretRows(fixture.bucketA, sourceServiceID, "gmailOAuth", "google")); err != nil {
		t.Fatalf("seed source application credentials: %v", err)
	}
	alternative := SecretKeyAlternative{
		Required:        targetKeys,
		AuthNames:       map[string]string{targetKeys[0]: "sheetsOAuth", targetKeys[1]: "sheetsOAuth"},
		AuthTypes:       map[string]string{targetKeys[0]: "oauth", targetKeys[1]: "oauth"},
		SourceServiceID: sourceServiceID, SourceAuthType: "oauth", SourceAuthName: "gmailOAuth",
	}
	selected, err := fixture.store.GetFirstCompleteSecretSet(fixture.ctx, fixture.bucketA, fixture.serviceID, []SecretKeyAlternative{alternative})
	assertOAuthApplicationReferenceSelection(t, selected, err, sourceServiceID, targetKeys)
	presence, err := fixture.store.(AppBucketReadinessStore).GetAppBucketCredentialPresence(fixture.ctx, fixture.bucketA, []AppCredentialRequirement{{
		ServiceID: fixture.serviceID, AuthType: "oauth", AuthName: "sheetsOAuth", SecretKeys: targetKeys,
		SourceServiceID: sourceServiceID, SourceAuthType: "oauth", SourceAuthName: "gmailOAuth",
	}})
	// Planning and runtime must agree on the same complete source pair.
	if err != nil || len(presence) != 1 || !presence[0].Connected {
		t.Fatalf("referenced OAuth application readiness = %#v err=%v", presence, err)
	}
}

// assertOAuthApplicationReferenceSelection validates the runtime projection for a referenced OAuth pair.
func assertOAuthApplicationReferenceSelection(t *testing.T, selected []WorkspaceSecret, err error, sourceServiceID uuid.UUID, targetKeys []string) {
	t.Helper()
	// Runtime receives source ciphertext under target key aliases without copying application credentials.
	if err != nil || len(selected) != 2 || selected[0].ServiceID != sourceServiceID || selected[0].KeyName != targetKeys[0] || selected[1].KeyName != targetKeys[1] {
		t.Fatalf("referenced OAuth application selection = %#v err=%v", selected, err)
	}
}

// oauthApplicationSecretRows builds one deterministic application pair for reference integration tests.
func oauthApplicationSecretRows(bucketID, serviceID uuid.UUID, authName, marker string) []WorkspaceSecret {
	return []WorkspaceSecret{
		{WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: bucketID, ServiceID: serviceID, KeyName: authName + "_client_id", CredentialType: "oauth"}, EncryptedDEK: "dek-id-" + marker, EncryptedValue: "value-id-" + marker},
		{WorkspaceSecretMeta: WorkspaceSecretMeta{BucketID: bucketID, ServiceID: serviceID, KeyName: authName + "_client_secret", CredentialType: "oauth"}, EncryptedDEK: "dek-secret-" + marker, EncryptedValue: "value-secret-" + marker},
	}
}

type workspaceSecretsIntegrationFixture struct {
	ctx       context.Context
	store     Store
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

// TestPostgresStore_WorkspaceSecrets exercises the bucket-scoped secret lifecycle against PostgreSQL.
func TestPostgresStore_WorkspaceSecrets(t *testing.T) {
	fixture := setupWorkspaceSecretsIntegrationFixture(t)
	secret, _ := seedWorkspaceSecretPair(t, fixture)
	assertOrderedActiveSecretSelection(t, fixture, secret)
	assertCredentialFamilyLifecycle(t, fixture, secret)
	assertWorkspaceSecretListUpdateDelete(t, fixture, secret)
	assertBucketSummaryLifecycle(t, fixture)
}

// setupWorkspaceSecretsIntegrationFixture creates isolated workspace, bucket, service, and version rows.
func setupWorkspaceSecretsIntegrationFixture(t *testing.T) workspaceSecretsIntegrationFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	// Integration coverage is optional when the developer has not supplied PostgreSQL.
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	// The returned fixture keeps using this context, so cancellation belongs to test cleanup rather than helper return.
	t.Cleanup(cancel)

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	// A configured integration database must be reachable before fixtures are mutated.
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	t.Cleanup(pool.Close)

	s := NewPostgresStore(pool)
	accountID := uuid.New()
	workspaceID := uuid.New()
	serviceID := uuid.New()
	bucketID := uuid.New()
	versionA, versionB := uuid.New(), uuid.New()

	execWorkspaceSecretFixtureSQL(t, pool, ctx, "DELETE FROM fused_workspaces")
	execWorkspaceSecretFixtureSQL(t, pool, ctx, "DELETE FROM fused_app_family_buckets")
	execWorkspaceSecretFixtureSQL(t, pool, ctx, "DELETE FROM fused_buckets")
	execWorkspaceSecretFixtureSQL(t, pool, ctx, "INSERT INTO fused_workspaces (id, account_id, name, slug) VALUES ($1, $2, $3, $4)", workspaceID, accountID, "Test WS", "test-ws")
	execWorkspaceSecretFixtureSQL(t, pool, ctx, "INSERT INTO fused_buckets (id, name) VALUES ($1, $2)", bucketID, "secrets-test-bucket-"+uuid.NewString())
	execWorkspaceSecretFixtureSQL(t, pool, ctx, `INSERT INTO fused_workspace_services (service_id, service_name) VALUES ($1, 'Test service')`, serviceID)
	execWorkspaceSecretFixtureSQL(t, pool, ctx, `
		INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version)
		VALUES ($1, $2, '2026-07-08'), ($1, $3, '2026-08-01')
	`, serviceID, versionA, versionB)
	return workspaceSecretsIntegrationFixture{ctx: ctx, store: s, bucketID: bucketID, serviceID: serviceID}
}

// execWorkspaceSecretFixtureSQL keeps integration setup failures uniform and immediately actionable.
func execWorkspaceSecretFixtureSQL(t *testing.T, pool *pgxpool.Pool, ctx context.Context, query string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, query, args...)
	// Setup must stop at the first partial fixture mutation to avoid misleading assertions.
	if err != nil {
		t.Fatalf("workspace secret fixture SQL failed: %v", err)
	}
}

// seedWorkspaceSecretPair inserts and verifies two independent secrets in one bucket.
func seedWorkspaceSecretPair(t *testing.T, fixture workspaceSecretsIntegrationFixture) (WorkspaceSecret, WorkspaceSecret) {
	t.Helper()
	sec1 := WorkspaceSecret{
		WorkspaceSecretMeta: WorkspaceSecretMeta{
			// A supplied value must never influence persistence; bucket ownership
			// is the single source of truth for this singleton Engine.
			BucketID:       fixture.bucketID,
			ServiceID:      fixture.serviceID,
			KeyName:        "API_KEY",
			CredentialType: "string",
		},
		EncryptedDEK:   "enc-dek",
		EncryptedValue: "enc-val",
	}
	// The initial insert must be readable through bucket ownership.
	if err := fixture.store.UpsertSecret(fixture.ctx, sec1); err != nil {
		t.Fatalf("UpsertSecret failed: %v", err)
	}
	metas, err := fixture.store.ListSecretMeta(fixture.ctx, fixture.bucketID)
	// Metadata must expose exactly the inserted bucket-owned secret.
	if err != nil || len(metas) != 1 || metas[0].BucketID != fixture.bucketID {
		t.Fatalf("secret workspace ownership = %#v, err=%v", metas, err)
	}

	sec2 := sec1
	sec2.KeyName = "API_SECRET"
	sec2.EncryptedValue = "enc-val-override"
	// A second key proves independent key storage within the same credential namespace.
	if err := fixture.store.UpsertSecret(fixture.ctx, sec2); err != nil {
		t.Fatalf("UpsertSecret 2 failed: %v", err)
	}
	return sec1, sec2
}

// assertOrderedActiveSecretSelection verifies required and optional expiry handling in alternative order.
func assertOrderedActiveSecretSelection(t *testing.T, fixture workspaceSecretsIntegrationFixture, secret WorkspaceSecret) {
	t.Helper()
	// The ordered selector must fall through an expired required branch and
	// return no expired optional value from the selected branch.
	expiredAt := time.Now().UTC().Add(-time.Minute)
	expiredRequired := secret
	expiredRequired.KeyName = "expired_required"
	expiredRequired.ExpiresAt = &expiredAt
	expiredOptional := secret
	expiredOptional.KeyName = "expired_optional"
	expiredOptional.ExpiresAt = &expiredAt
	// Both expired fixtures must exist so selection, rather than absence, drives fallback.
	if err := fixture.store.UpsertSecrets(fixture.ctx, []WorkspaceSecret{expiredRequired, expiredOptional}); err != nil {
		t.Fatalf("UpsertSecrets expired selector fixtures: %v", err)
	}
	selected, err := fixture.store.GetFirstCompleteSecretSet(fixture.ctx, fixture.bucketID, fixture.serviceID, []SecretKeyAlternative{
		{Required: []string{"expired_required"}},
		{Required: []string{"API_SECRET"}, Optional: []string{"expired_optional"}},
	})
	// Selection must skip the expired required branch and omit expired optional material.
	if err != nil || len(selected) != 1 || selected[0].KeyName != "API_SECRET" {
		t.Fatalf("ordered active secret selection=%#v err=%v", selected, err)
	}
	// Expiry fixtures are removed before lifecycle counts are asserted.
	if err := fixture.store.DeleteSecrets(fixture.ctx, fixture.bucketID, fixture.serviceID, []string{"expired_required", "expired_optional"}); err != nil {
		t.Fatalf("delete selector fixtures: %v", err)
	}
}

// assertWorkspaceSecretListUpdateDelete covers listing, idempotent update, and exact-key deletion.
func assertWorkspaceSecretListUpdateDelete(t *testing.T, fixture workspaceSecretsIntegrationFixture, secret WorkspaceSecret) {
	t.Helper()
	assertWorkspaceSecretListAndUpdate(t, fixture, secret)
	assertWorkspaceSecretExactDelete(t, fixture)
}

// assertWorkspaceSecretListAndUpdate verifies exact listing and idempotent replacement cardinality.
func assertWorkspaceSecretListAndUpdate(t *testing.T, fixture workspaceSecretsIntegrationFixture, secret WorkspaceSecret) {
	t.Helper()
	metas, err := fixture.store.ListSecretMeta(fixture.ctx, fixture.bucketID)
	// Both independently stored keys must appear in metadata.
	if err != nil || len(metas) != 2 {
		t.Fatalf("ListSecretMeta failed: %v, len=%d", err, len(metas))
	}

	secrets, err := fixture.store.ListSecretsForBucket(fixture.ctx, fixture.bucketID, fixture.serviceID)
	// Ciphertext listing must preserve the same exact service-scoped pair.
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket failed: %v, len=%d", err, len(secrets))
	}

	secret.EncryptedValue = "enc-val-updated"
	// Upsert updates the exact key rather than creating a duplicate.
	if err := fixture.store.UpsertSecret(fixture.ctx, secret); err != nil {
		t.Fatalf("UpsertSecret update failed: %v", err)
	}

	secrets, err = fixture.store.ListSecretsForBucket(fixture.ctx, fixture.bucketID, fixture.serviceID)
	// Updating one key must leave the two-key family cardinality unchanged.
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket after update failed: %v", err)
	}
}

// assertWorkspaceSecretExactDelete verifies deleting one key preserves its independent sibling.
func assertWorkspaceSecretExactDelete(t *testing.T, fixture workspaceSecretsIntegrationFixture) {
	t.Helper()
	// Deletion must succeed before the surviving sibling can prove the operation was exact.
	if err := fixture.store.DeleteSecret(fixture.ctx, fixture.bucketID, fixture.serviceID, "API_KEY"); err != nil {
		t.Fatalf("DeleteSecret failed: %v", err)
	}
	secrets, err := fixture.store.ListSecretsForBucket(fixture.ctx, fixture.bucketID, fixture.serviceID)
	// The sibling API_SECRET must survive independent deletion.
	if err != nil || len(secrets) != 1 {
		t.Fatalf("ListSecretsForBucket after delete expected 1, got %d", len(secrets))
	}
}

// assertCredentialFamilyLifecycle verifies paired credentials list and delete as one administrative unit.
func assertCredentialFamilyLifecycle(t *testing.T, fixture workspaceSecretsIntegrationFixture, secret WorkspaceSecret) {
	t.Helper()
	basicUsername := secret
	basicUsername.KeyName = "basicAuth_username"
	basicUsername.CredentialType = "basic"
	basicPassword := basicUsername
	basicPassword.KeyName = "basicAuth_password"
	// Both halves are written atomically so the family cannot become partially ready.
	if err := fixture.store.UpsertSecrets(fixture.ctx, []WorkspaceSecret{basicUsername, basicPassword}); err != nil {
		t.Fatalf("UpsertSecrets basic pair failed: %v", err)
	}
	page, total, err := fixture.store.ListSecretMetaPage(fixture.ctx, fixture.bucketID, 10, 0)
	// Pagination must return the family aggregate without query failure.
	if err != nil {
		t.Fatalf("ListSecretMetaPage failed: %v", err)
	}
	// Two single keys and one Basic family produce three administrative rows.
	if total != 3 {
		t.Fatalf("ListSecretMetaPage total = %d, want two single secrets and one Basic family", total)
	}
	assertCredentialFamily(t, page, "basicAuth", []string{"basicAuth_password", "basicAuth_username"})
	// Family deletion removes the pair in one store operation.
	if err := fixture.store.DeleteSecrets(fixture.ctx, fixture.bucketID, fixture.serviceID, []string{"basicAuth_username", "basicAuth_password"}); err != nil {
		t.Fatalf("DeleteSecrets basic pair failed: %v", err)
	}
	secrets, err := fixture.store.ListSecretsForBucket(fixture.ctx, fixture.bucketID, fixture.serviceID)
	// Both independent API keys remain after the family-only deletion.
	if err != nil || len(secrets) != 2 {
		t.Fatalf("ListSecretsForBucket after family delete: %v, len=%d", err, len(secrets))
	}
}

// assertBucketSummaryLifecycle verifies set-based bucket counts for one secret and one generic value.
func assertBucketSummaryLifecycle(t *testing.T, fixture workspaceSecretsIntegrationFixture) {
	t.Helper()
	// The value insert must succeed before aggregate counts can establish the set-based summary invariant.
	if err := fixture.store.UpsertBucketValue(fixture.ctx, BucketValue{
		BucketID:  fixture.bucketID,
		ServiceID: fixture.serviceID,
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
	values, err := fixture.store.ListBucketValues(fixture.ctx, fixture.bucketID)
	if err != nil || len(values) != 1 || values[0].KeyName != "X-Region" {
		t.Fatalf("ListBucketValues after upsert = %#v, err=%v", values, err)
	}
	summaries, total, err := fixture.store.ListBucketSummaries(fixture.ctx, 10, 0)
	// Summary retrieval must succeed before its aggregate cardinality is inspected.
	if err != nil {
		t.Fatalf("ListBucketSummaries failed: %v", err)
	}
	// The isolated fixture owns exactly one bucket, independent of its contents.
	if total != 1 {
		t.Fatalf("ListBucketSummaries total = %d, want 1", total)
	}
	// The remaining secret and generic value must each contribute once to their aggregate column.
	if len(summaries) != 1 || summaries[0].SecretCount != 1 || summaries[0].ValueCount != 1 {
		t.Fatalf("ListBucketSummaries counts = %#v, want one secret and one value", summaries)
	}
}

// assertCredentialFamily locates one aggregate and verifies its stable member ordering.
func assertCredentialFamily(t *testing.T, metas []WorkspaceSecretMeta, keyName string, keyNames []string) {
	t.Helper()
	// Only the requested aggregate should participate in the member checks.
	for _, meta := range metas {
		// Unrelated singleton and family rows remain valid page results and must be skipped.
		if meta.KeyName == keyName {
			// Cardinality must match before indexed member comparison is safe.
			if len(meta.KeyNames) != len(keyNames) {
				t.Fatalf("credential family keys = %#v, want %#v", meta.KeyNames, keyNames)
			}
			// Stable ordering makes the aggregate deterministic for API and UI consumers.
			for index := range keyNames {
				// Every position must preserve the store's canonical family ordering.
				if meta.KeyNames[index] != keyNames[index] {
					t.Fatalf("credential family keys = %#v, want %#v", meta.KeyNames, keyNames)
				}
			}
			return
		}
	}
	t.Fatalf("credential family %q not found in %#v", keyName, metas)
}
