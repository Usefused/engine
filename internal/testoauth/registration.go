// Package testoauth provides canonical bucket and contract fixtures for delegated OAuth integration tests.
package testoauth

import (
	"context"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Seed persists the same bucket pair and service snapshot used by ordinary Engine OAuth.
func Seed(t *testing.T, pool *pgxpool.Pool, serviceID uuid.UUID, auth fusedobject.AuthConfig, key []byte, clientID, secret string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	runtime := store.NewPostgresStore(pool)
	bucket, err := runtime.CreateBucket(t.Context(), "published-oauth-"+uuid.NewString(), false)
	require(t, err)
	version := uuid.New()
	// Cleanup removes only this fixture's independently allocated material.
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_oauth_publications WHERE bucket_id=$1`, bucket.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id=$1`, bucket.ID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id=$1`, version)
	})
	SavePair(t, runtime, bucket.ID, serviceID, auth.Name, key, clientID, secret)
	snapshot := store.ServiceContractSnapshot{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(), ServiceID: serviceID, ServiceVersionID: version, Version: "1.0.0", ServiceMetadata: fusedobject.ServiceMetadata{ID: serviceID, ServiceVersionID: version, Name: "OAuth fixture", AuthConfigs: fusedobject.AuthConfigs{auth}}}
	_, err = runtime.(store.ServiceContractSnapshotStore).UpsertServiceContractSnapshot(t.Context(), snapshot)
	require(t, err)
	return bucket.ID, version
}

// SavePair uses the ordinary encrypted bucket writer so rotation tests do not simulate a second credential store.
func SavePair(t *testing.T, runtime store.Store, bucket, service uuid.UUID, name string, key []byte, clientID, secret string) {
	t.Helper()
	idKey, secretKey, ok := credentialkeys.OAuthApplication(name)
	// A malformed fixture must fail rather than create unrelated bucket keys.
	if !ok {
		t.Fatal("invalid OAuth fixture name")
	}
	var rows []store.WorkspaceSecret
	for _, value := range []struct{ key, value string }{{idKey, clientID}, {secretKey, secret}} {
		wrapped, dek, err := store.WrapDEK(key)
		require(t, err)
		encrypted, err := store.EncryptWithDEK(dek, value.value)
		require(t, err)
		rows = append(rows, store.WorkspaceSecret{WorkspaceSecretMeta: store.WorkspaceSecretMeta{BucketID: bucket, ServiceID: service, KeyName: value.key, CredentialType: "oauth"}, EncryptedDEK: wrapped, EncryptedValue: encrypted})
	}
	require(t, runtime.UpsertSecrets(t.Context(), rows))
}

// require keeps fixture setup failures distinct from successful security denial.
func require(t *testing.T, err error) {
	t.Helper()
	// Failed setup cannot produce a valid test of an authorization boundary.
	if err != nil {
		t.Fatal(err)
	}
}
