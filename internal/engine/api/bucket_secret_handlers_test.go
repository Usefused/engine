package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestUpsertBucketSecretHandlerStoresGenericSecret verifies the route creates
// the same service-independent row consumed by bucket secret references.
func TestUpsertBucketSecretHandlerStoresGenericSecret(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	body, err := json.Marshal(BucketSecretUpsertPayload{KeyName: "webhook_signing", Value: "secret-token", ExpiresAt: &expiresAt})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.otherBucketID.String()+"/secrets", bytes.NewReader(body))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedSecret == nil {
		t.Fatal("expected bucket secret to be persisted")
	}
	stored := fixture.store.upsertedSecret
	// Nil service identity is the storage invariant that keeps generic secrets reusable across services and webhooks.
	if stored.BucketID != fixture.otherBucketID || stored.ServiceID != uuid.Nil || stored.KeyName != "secret:webhook_signing" || stored.CredentialType != "bucket_secret" {
		t.Fatalf("stored bucket secret metadata = %#v", stored.WorkspaceSecretMeta)
	}
	if stored.ExpiresAt == nil || !stored.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("stored expiry = %v, want %s", stored.ExpiresAt, expiresAt)
	}
	if stored.EncryptedValue == "" || stored.EncryptedValue == "secret-token" {
		t.Fatal("bucket secret value was not encrypted before persistence")
	}
}

// TestUpsertBucketSecretHandlerRejectsUnsafeInput verifies malformed generic
// identities and values fail before any encrypted row is stored.
func TestUpsertBucketSecretHandlerRejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{name: "empty value", body: `{"key_name":"webhook_signing","value":""}`, code: "empty_bucket_secret_value"},
		{name: "multi segment name", body: `{"key_name":"webhook.signing","value":"secret-token"}`, code: "invalid_bucket_secret_name"},
		{name: "unknown field", body: `{"key_name":"webhook_signing","value":"secret-token","service_id":"` + uuid.NewString() + `"}`, code: "invalid_bucket_secret_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSecretsFixture()
			router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
			req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.otherBucketID.String()+"/secrets", bytes.NewBufferString(test.body))
			req.Header.Set("X-API-Key", "test-key")
			rr := httptest.NewRecorder()
			router.ServeHTTP(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
			}
			var envelope workspaceConfigErrorResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != test.code {
				t.Fatalf("error envelope = %#v, decode error=%v", envelope, err)
			}
			// Admission failures must prove no secret write was attempted.
			if envelope.Error.Phase != "bucket_secret_upsert" || envelope.Error.CommitState != "not_committed" || fixture.store.upsertedSecret != nil {
				t.Fatalf("unexpected mutation result: error=%#v stored=%#v", envelope.Error, fixture.store.upsertedSecret)
			}
		})
	}
}

// TestUpsertBucketSecretHandlerRequiresExplicitBucket verifies the new surface
// never falls back to the workspace default bucket.
func TestUpsertBucketSecretHandlerRequiresExplicitBucket(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/not-a-uuid/secrets", bytes.NewBufferString(`{"key_name":"webhook_signing","value":"secret-token"}`))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest || fixture.store.upsertedSecret != nil {
		t.Fatalf("explicit bucket rejection status=%d stored=%#v body=%s", rr.Code, fixture.store.upsertedSecret, rr.Body.String())
	}
}
