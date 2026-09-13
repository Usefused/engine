package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

// TestUpsertBucketSecretHandlerPersistsToPostgres drives the Engine HTTP route
// through production storage and verifies its encrypted generic-secret row.
func TestUpsertBucketSecretHandlerPersistsToPostgres(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// PostgreSQL coverage is opt-in so ordinary unit runs never guess or mutate a developer database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Schema initialization must succeed before this test creates isolated fixture rows.
	if err != nil {
		t.Fatalf("initialize Engine PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)
	repository := store.NewPostgresStore(pool)
	bucket, err := repository.CreateBucket(ctx, "bucket-secret-http-"+uuid.NewString(), false)
	// A real bucket row is required because the production upsert derives ownership through its foreign key.
	if err != nil {
		t.Fatalf("create PostgreSQL bucket fixture: %v", err)
	}
	t.Cleanup(func() {
		// Deleting this generated bucket cascades only the secret rows owned by this fixture.
		if _, cleanupErr := pool.Exec(context.Background(), `DELETE FROM fused_buckets WHERE id=$1`, bucket.ID); cleanupErr != nil {
			t.Errorf("clean PostgreSQL bucket-secret fixture: %v", cleanupErr)
		}
	})

	masterKey := []byte("12345678901234567890123456789012")
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	body, err := json.Marshal(BucketSecretUpsertPayload{KeyName: "webhook_signing", Value: "postgres-secret-token", ExpiresAt: &expiresAt})
	// Encoding failure would invalidate the HTTP acceptance setup before Engine is exercised.
	if err != nil {
		t.Fatalf("encode bucket secret request: %v", err)
	}
	router := buildConnectAdminRouter(repository, uuid.New(), masterKey)
	request := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+bucket.ID.String()+"/secrets", bytes.NewReader(body))
	request.Header.Set("X-API-Key", "postgres-test-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	// A no-content response proves the real store accepted the complete Engine mutation.
	if response.Code != http.StatusNoContent {
		t.Fatalf("bucket secret HTTP status=%d body=%s", response.Code, response.Body.String())
	}

	var serviceID uuid.UUID
	var keyName, credentialType, wrappedDEK, encryptedValue string
	var storedExpiry *time.Time
	err = pool.QueryRow(ctx, `
		SELECT service_id, key_name, credential_type, encrypted_dek, encrypted_value, expires_at
		FROM fused_workspace_secrets
		WHERE bucket_id=$1 AND service_id=$2 AND key_name=$3
	`, bucket.ID, uuid.Nil, "secret:webhook_signing").Scan(&serviceID, &keyName, &credentialType, &wrappedDEK, &encryptedValue, &storedExpiry)
	// Direct SQL inspection verifies the handler reached PostgreSQL with the generic storage identity.
	if err != nil {
		t.Fatalf("read persisted bucket secret: %v", err)
	}
	if serviceID != uuid.Nil || keyName != "secret:webhook_signing" || credentialType != "bucket_secret" {
		t.Fatalf("persisted bucket secret identity service=%s key=%q type=%q", serviceID, keyName, credentialType)
	}
	if wrappedDEK == "" || encryptedValue == "" || encryptedValue == "postgres-secret-token" {
		t.Fatal("PostgreSQL row does not contain encrypted secret material")
	}
	if storedExpiry == nil || !storedExpiry.Equal(expiresAt) {
		t.Fatalf("persisted expiry=%v want=%s", storedExpiry, expiresAt)
	}
	dek, err := store.UnwrapDEK(masterKey, wrappedDEK)
	// The stored data-encryption key must remain recoverable only with the Engine master key.
	if err != nil {
		t.Fatalf("unwrap persisted bucket secret DEK: %v", err)
	}
	plaintext, err := store.DecryptWithDEK(dek, encryptedValue)
	// A successful round trip proves ciphertext was not merely transformed into unusable data.
	if err != nil {
		t.Fatalf("decrypt persisted bucket secret: %v", err)
	}
	if plaintext != "postgres-secret-token" {
		t.Fatal("decrypted PostgreSQL value did not match the submitted secret")
	}
}
