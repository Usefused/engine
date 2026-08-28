package store

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/google/uuid"
)

// TestLegacyConnectConfigCutoverMigratesAndNoOps covers live, fresh, and already-upgraded startup behavior.
func TestLegacyConnectConfigCutoverMigratesAndNoOps(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	authName := "primaryOAuth"
	seedLegacyConnectConfigForCutover(t, fixture, authName)
	migrator := fixture.store.(LegacyConnectConfigCutoverStore)
	result := runLegacyConnectConfigCutoverForTest(t, fixture, migrator)
	assertInitialLegacyConnectConfigCutover(t, result)
	assertMigratedOAuthApplicationValues(t, fixture, authName)
	assertLegacyConnectConfigTableAbsent(t, fixture)
	recreateLegacyConnectConfigTableForCutoverTest(t, fixture)
	again := runLegacyConnectConfigCutoverForTest(t, fixture, migrator)
	assertRepeatedLegacyConnectConfigCutover(t, again)
	assertLegacyConnectConfigTableAbsent(t, fixture)
}

// TestLegacyConnectConfigCutoverKeepsAmbiguousAndDisabledRowsInactive exercises fail-closed historical rows in PostgreSQL.
func TestLegacyConnectConfigCutoverKeepsAmbiguousAndDisabledRowsInactive(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	uniqueServiceID, ambiguousServiceID, disabledServiceID := uuid.New(), uuid.New(), uuid.New()
	seedLegacyCutoverAuthContract(t, fixture, uniqueServiceID, []string{"uniqueOAuth"})
	seedLegacyCutoverAuthContract(t, fixture, ambiguousServiceID, []string{"firstOAuth", "secondOAuth"})
	seedLegacyConnectConfigRowForCutover(t, fixture, uniqueServiceID, "", true)
	seedLegacyConnectConfigRowForCutover(t, fixture, ambiguousServiceID, "", true)
	seedLegacyConnectConfigRowForCutover(t, fixture, disabledServiceID, "disabledOAuth", false)

	result := runLegacyConnectConfigCutoverForTest(t, fixture, fixture.store.(LegacyConnectConfigCutoverStore))
	// Only the uniquely identified enabled family becomes active bucket secret material.
	if result.MigratedRows != 1 || result.SkippedRows != 2 || result.BatchCount != 3 {
		t.Fatalf("cutover result = %#v", result)
	}
	assertLegacyCutoverSecretPresence(t, fixture, uniqueServiceID, "uniqueOAuth", true)
	assertLegacyCutoverSecretPresence(t, fixture, ambiguousServiceID, "firstOAuth", false)
	assertLegacyCutoverSecretPresence(t, fixture, disabledServiceID, "disabledOAuth", false)
}

// TestLegacyConnectConfigCutoverSeparatesSameServiceAuthFamilies proves one migration page cannot conflate OAuth and OIDC registrations across buckets.
func TestLegacyConnectConfigCutoverSeparatesSameServiceAuthFamilies(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	serviceID := uuid.New()
	seedLegacyCutoverRawAuthContract(t, fixture, serviceID, `[
		{"type":"oauth2_authorization_code","name":"primaryOAuth"},
		{"type":"openidConnect","name":"primaryOIDC"}
	]`)
	seedLegacyConnectConfigIdentityForCutover(t, fixture, fixture.bucketA, serviceID, "oauth2-authorization-code", "", true)
	seedLegacyConnectConfigIdentityForCutover(t, fixture, fixture.bucketB, serviceID, "openidConnect", "", true)

	result := runLegacyConnectConfigCutoverBatchForTest(t, fixture, fixture.store.(LegacyConnectConfigCutoverStore), 2)
	// Both rows sharing one service must resolve within their own canonical family in the same SQL batch.
	if result.MigratedRows != 2 || result.SkippedRows != 0 || result.BatchCount != 1 {
		t.Fatalf("same-service cutover result = %#v", result)
	}
	assertLegacyCutoverSecretPresenceInBucket(t, fixture, fixture.bucketA, serviceID, "primaryOAuth", true)
	assertLegacyCutoverSecretPresenceInBucket(t, fixture, fixture.bucketA, serviceID, "primaryOIDC", false)
	assertLegacyCutoverSecretPresenceInBucket(t, fixture, fixture.bucketB, serviceID, "primaryOIDC", true)
	assertLegacyCutoverSecretPresenceInBucket(t, fixture, fixture.bucketB, serviceID, "primaryOAuth", false)
}

// TestLegacyConnectConfigCutoverTreatsNonArrayAuthConfigsAsUnresolved proves malformed metadata fails closed without aborting the permanent cutover.
func TestLegacyConnectConfigCutoverTreatsNonArrayAuthConfigsAsUnresolved(t *testing.T) {
	fixture := setupConnectAuthStore(t)
	nullServiceID, objectServiceID := uuid.New(), uuid.New()
	seedLegacyCutoverRawAuthContract(t, fixture, nullServiceID, `null`)
	seedLegacyCutoverRawAuthContract(t, fixture, objectServiceID, `{"type":"oauth2","name":"not-an-array"}`)
	seedLegacyConnectConfigRowForCutover(t, fixture, nullServiceID, "", true)
	seedLegacyConnectConfigRowForCutover(t, fixture, objectServiceID, "", true)

	result := runLegacyConnectConfigCutoverBatchForTest(t, fixture, fixture.store.(LegacyConnectConfigCutoverStore), 2)
	// Invalid metadata never selects a scheme, but it also cannot trigger jsonb_array_elements type errors.
	if result.MigratedRows != 0 || result.SkippedRows != 2 || result.BatchCount != 1 {
		t.Fatalf("non-array auth-config cutover result = %#v", result)
	}
}

// seedLegacyCutoverAuthContract stores one enabled pinned version with the supplied compatible OAuth names.
func seedLegacyCutoverAuthContract(t *testing.T, fixture connectAuthFixture, serviceID uuid.UUID, authNames []string) {
	t.Helper()
	versionID := uuid.New()
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, serviceID, "cutover-"+serviceID.String(), "v1", versionID, "Cutover service", fixture.accountID); err != nil {
		t.Fatalf("activate cutover service: %v", err)
	}
	authJSON := "["
	for index, name := range authNames {
		// Auth objects are assembled from fixed test values and generated UUID-owned names only.
		if index > 0 {
			authJSON += ","
		}
		authJSON += `{"type":"oauth2","name":"` + name + `"}`
	}
	authJSON += "]"
	_, err := fixture.store.(*postgresStore).db.Exec(fixture.ctx, `INSERT INTO fused_service_contract_snapshots
		(service_id, service_version_id, version, contract_version, required_capabilities, contract_hash, service_metadata)
		VALUES ($1, $2, 'v1', 1, '{}', $3, jsonb_build_object('auth_configs', $4::jsonb))`,
		serviceID, versionID, "sha256:"+strings.Repeat("a", 64), authJSON)
	if err != nil {
		t.Fatalf("seed cutover auth contract: %v", err)
	}
}

// seedLegacyCutoverRawAuthContract stores an exact auth_configs JSON value so PostgreSQL tests can cover valid and malformed historical metadata.
func seedLegacyCutoverRawAuthContract(t *testing.T, fixture connectAuthFixture, serviceID uuid.UUID, authJSON string) {
	t.Helper()
	versionID := uuid.New()
	// The snapshot must be pinned through ordinary workspace activation before raw metadata is inserted.
	if err := fixture.store.AddWorkspaceServiceVersion(fixture.ctx, serviceID, "cutover-"+serviceID.String(), "v1", versionID, "Cutover service", fixture.accountID); err != nil {
		t.Fatalf("activate raw cutover service: %v", err)
	}
	_, err := fixture.store.(*postgresStore).db.Exec(fixture.ctx, `INSERT INTO fused_service_contract_snapshots
		(service_id, service_version_id, version, contract_version, required_capabilities, contract_hash, service_metadata)
		VALUES ($1, $2, 'v1', 1, '{}', $3, jsonb_build_object('auth_configs', $4::jsonb))`,
		serviceID, versionID, "sha256:"+strings.Repeat("b", 64), authJSON)
	// Invalid fixture metadata is deliberate JSON, while invalid JSON must still fail setup.
	if err != nil {
		t.Fatalf("seed raw cutover auth contract: %v", err)
	}
}

// assertLegacyCutoverSecretPresence checks whether one historical family became executable secret material.
func assertLegacyCutoverSecretPresence(t *testing.T, fixture connectAuthFixture, serviceID uuid.UUID, authName string, want bool) {
	t.Helper()
	assertLegacyCutoverSecretPresenceInBucket(t, fixture, fixture.bucketA, serviceID, authName, want)
}

// assertLegacyCutoverSecretPresenceInBucket checks one exact bucket-owned application family after cutover.
func assertLegacyCutoverSecretPresenceInBucket(t *testing.T, fixture connectAuthFixture, bucketID, serviceID uuid.UUID, authName string, want bool) {
	t.Helper()
	clientIDKey, clientSecretKey, _ := credentialkeys.OAuthApplication(authName)
	secrets, err := fixture.store.GetSecrets(fixture.ctx, bucketID, serviceID, []string{clientIDKey, clientSecretKey})
	// A failed exact read cannot be interpreted as credential absence.
	if err != nil {
		t.Fatalf("read cutover secrets: %v", err)
	}
	// Complete pairs are the only active outcome; zero rows proves skipped ciphertext was not materialized.
	if (len(secrets) == 2) != want {
		t.Fatalf("secret presence service=%s auth=%q rows=%d want=%t", serviceID, authName, len(secrets), want)
	}
}

// runLegacyConnectConfigCutoverForTest executes the permanent migration with the smallest valid batch.
func runLegacyConnectConfigCutoverForTest(t *testing.T, fixture connectAuthFixture, migrator LegacyConnectConfigCutoverStore) LegacyConnectConfigCutoverResult {
	t.Helper()
	return runLegacyConnectConfigCutoverBatchForTest(t, fixture, migrator, 1)
}

// runLegacyConnectConfigCutoverBatchForTest executes the permanent migration with an explicit batch size for collision coverage.
func runLegacyConnectConfigCutoverBatchForTest(t *testing.T, fixture connectAuthFixture, migrator LegacyConnectConfigCutoverStore, batchSize int) LegacyConnectConfigCutoverResult {
	t.Helper()
	result, err := migrator.MigrateLegacyConnectConfigs(fixture.ctx, connectAuthTestMasterKey, batchSize)
	// Migration errors must retain the fixture context rather than being folded into result assertions.
	if err != nil {
		t.Fatalf("MigrateLegacyConnectConfigs: %v", err)
	}
	return result
}

// assertInitialLegacyConnectConfigCutover checks the bounded first-application aggregate.
func assertInitialLegacyConnectConfigCutover(t *testing.T, result LegacyConnectConfigCutoverResult) {
	t.Helper()
	// One bounded batch must fully replace the single legacy registration.
	if result.MigratedRows != 1 || result.BatchCount != 1 || result.AlreadyDone {
		t.Fatalf("cutover result = %#v", result)
	}
}

// assertMigratedOAuthApplicationValues decrypts only test fixtures to prove the live values survived.
func assertMigratedOAuthApplicationValues(t *testing.T, fixture connectAuthFixture, authName string) {
	t.Helper()
	clientIDKey, clientSecretKey, _ := credentialkeys.OAuthApplication(authName)
	secrets, err := fixture.store.GetSecrets(fixture.ctx, fixture.bucketA, fixture.serviceID, []string{clientIDKey, clientSecretKey})
	// Both deterministic rows must be present before decrypting their fixture values.
	if err != nil || len(secrets) != 2 {
		t.Fatalf("migrated secrets = %#v, err=%v", secrets, err)
	}
	values := map[string]string{}
	for _, secret := range secrets {
		values[secret.KeyName] = decryptConnectAuthValue(t, secret.EncryptedDEK, secret.EncryptedValue)
	}
	// The cutover changes storage only; provider application values remain byte-for-byte stable.
	if values[clientIDKey] != "client-id-v1" || values[clientSecretKey] != "client-secret-v1" {
		t.Fatalf("migrated values did not round-trip")
	}
}

// assertLegacyConnectConfigTableAbsent proves runtime startup leaves no competing credential table.
func assertLegacyConnectConfigTableAbsent(t *testing.T, fixture connectAuthFixture) {
	t.Helper()
	var tableExists bool
	// Query and presence are asserted together because either outcome makes startup compatibility ambiguous.
	if err := fixture.store.(*postgresStore).db.QueryRow(fixture.ctx, `SELECT to_regclass('fused_connect_configs') IS NOT NULL`).Scan(&tableExists); err != nil || tableExists {
		t.Fatalf("legacy table exists=%v err=%v", tableExists, err)
	}
}

// assertRepeatedLegacyConnectConfigCutover checks permanent-ledger restart behavior.
func assertRepeatedLegacyConnectConfigCutover(t *testing.T, result LegacyConnectConfigCutoverResult) {
	t.Helper()
	// The permanent ledger removes an empty compatibility table recreated by immutable schema history.
	if !result.AlreadyDone || result.MigratedRows != 0 {
		t.Fatalf("second cutover = %#v", result)
	}
}

// recreateLegacyConnectConfigTableForCutoverTest models immutable schema setup on a later Engine restart.
func recreateLegacyConnectConfigTableForCutoverTest(t *testing.T, fixture connectAuthFixture) {
	t.Helper()
	_, err := fixture.store.(*postgresStore).db.Exec(fixture.ctx, `CREATE TABLE fused_connect_configs (id uuid PRIMARY KEY)`)
	// A failed fixture recreation cannot exercise the permanent already-applied cleanup path.
	if err != nil {
		t.Fatalf("recreate legacy connect config table: %v", err)
	}
}

// seedLegacyConnectConfigForCutover writes the historical encrypted shape without restoring a runtime store API.
func seedLegacyConnectConfigForCutover(t *testing.T, fixture connectAuthFixture, authName string) {
	t.Helper()
	seedLegacyConnectConfigRowForCutover(t, fixture, fixture.serviceID, authName, true)
}

// seedLegacyConnectConfigRowForCutover writes one configurable historical encrypted row.
func seedLegacyConnectConfigRowForCutover(t *testing.T, fixture connectAuthFixture, serviceID uuid.UUID, authName string, enabled bool) {
	t.Helper()
	seedLegacyConnectConfigIdentityForCutover(t, fixture, fixture.bucketA, serviceID, "oauth", authName, enabled)
}

// seedLegacyConnectConfigIdentityForCutover writes one historical row under an exact bucket and authored auth spelling.
func seedLegacyConnectConfigIdentityForCutover(t *testing.T, fixture connectAuthFixture, bucketID, serviceID uuid.UUID, authType, authName string, enabled bool) {
	t.Helper()
	wrappedDEK, dek, err := WrapDEK(connectAuthTestMasterKey)
	// Fixture encryption must retain the same master-key envelope expected by the cutover.
	if err != nil {
		t.Fatalf("wrap legacy DEK: %v", err)
	}
	clientID, err := EncryptWithDEK(dek, "client-id-v1")
	// Either application value failing encryption invalidates the complete historical row.
	if err != nil {
		t.Fatalf("encrypt legacy client ID: %v", err)
	}
	clientSecret, err := EncryptWithDEK(dek, "client-secret-v1")
	// The paired secret must share the historical row DEK so migration exercises real decoding.
	if err != nil {
		t.Fatalf("encrypt legacy client secret: %v", err)
	}
	_, err = fixture.store.(*postgresStore).db.Exec(fixture.ctx, `INSERT INTO fused_connect_configs
		(bucket_id, service_id, auth_type, auth_name, enabled, encrypted_dek, encrypted_client_id, encrypted_client_secret, redirect_uri)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'https://discarded.invalid/callback')`,
		bucketID, serviceID, authType, authName, enabled, wrappedDEK, clientID, clientSecret)
	// A failed seed cannot provide evidence about the cutover outcome.
	if err != nil {
		t.Fatalf("seed legacy connect config: %v", err)
	}
}
