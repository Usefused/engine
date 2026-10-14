package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type secretExportTestStore struct {
	store.Store
	secret        *store.WorkspaceSecret
	reads, writes int
}

// GetExportableSecret models the production point lookup without using runtime credential caches.
func (s *secretExportTestStore) GetExportableSecret(_ context.Context, bucketID, secretID uuid.UUID) (*store.WorkspaceSecret, error) {
	s.reads++
	// Only the current exact opted-in row can be revealed.
	if s.secret == nil || s.secret.BucketID != bucketID || s.secret.ID != secretID || !s.secret.Exportable {
		return nil, nil
	}
	return s.secret, nil
}

// SetSecretExportable models policy updates separately from value replacement.
func (s *secretExportTestStore) SetSecretExportable(_ context.Context, bucketID, secretID uuid.UUID, enabled bool) (bool, error) {
	s.writes++
	// A wrong bucket must never target this fixture's stored value.
	if s.secret == nil || s.secret.BucketID != bucketID || s.secret.ID != secretID {
		return false, nil
	}
	s.secret.Exportable = enabled
	return true, nil
}

// secretExportTestRequest mounts real handlers with an explicit snapshot grant and exact route identity.
func secretExportTestRequest(t *testing.T, repository store.Store, key []byte, bucketID, secretID uuid.UUID, permission accesscontrol.Permission, method, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	actor := actorWithResourcePermissions(t, uuid.New(), accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}})
	router := chi.NewRouter()
	router.Post("/buckets/{bucket_id}/secrets/{secret_id}/reveal", RevealBucketSecretHandler(repository, key))
	router.Put("/buckets/{bucket_id}/secrets/{secret_id}/exportable", SetBucketSecretExportableHandler(repository))
	request := httptest.NewRequest(method, "/buckets/"+bucketID.String()+"/secrets/"+secretID.String()+"/"+action, strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

// TestSecretRevealPermissionAndRevocation verifies real encryption, separate grants, and current-policy checks.
func TestSecretRevealPermissionAndRevocation(t *testing.T) {
	fixture := newSecretsFixture()
	secret, err := buildEncryptedSecret(fixture.otherBucketID, SecretUpsertPayload{ServiceID: fixture.serviceID, KeyName: "key", CredentialType: "api_key", Value: "synthetic-api-key", Exportable: true}, fixture.masterKey)
	// Fixture encryption must use the same envelope format as production writes.
	if err != nil {
		t.Fatal(err)
	}
	secret.ID = uuid.New()
	repository := &secretExportTestStore{secret: &secret}
	for _, permission := range []accesscontrol.Permission{accesscontrol.PermissionCredentialsManage, accesscontrol.PermissionBucketUse, accesscontrol.PermissionCredentialsMetadataRead} {
		response := secretExportTestRequest(t, repository, fixture.masterKey, secret.BucketID, secret.ID, permission, "POST", "reveal", "")
		// Neither metadata reads, management, nor runtime use implies reveal authority.
		if response.Code != 403 || repository.reads != 0 {
			t.Fatalf("permission %s: status=%d reads=%d", permission, response.Code, repository.reads)
		}
	}
	response := secretExportTestRequest(t, repository, fixture.masterKey, secret.BucketID, secret.ID, accesscontrol.PermissionCredentialsReveal, "POST", "reveal", "")
	var result map[string]string
	_ = json.Unmarshal(response.Body.Bytes(), &result)
	// Authorized disclosure preserves exact value bytes and disables caching.
	if response.Code != 200 || result["value"] != "synthetic-api-key" || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("reveal status=%d", response.Code)
	}
	assertSecretRevealRevocation(t, repository, fixture, secret)
}

// TestSecretRevealRejectsWrongBucketAndCorruptMaterial verifies failures cannot return partial plaintext.
func TestSecretRevealRejectsWrongBucketAndCorruptMaterial(t *testing.T) {
	secret := &store.WorkspaceSecret{WorkspaceSecretMeta: store.WorkspaceSecretMeta{ID: uuid.New(), BucketID: uuid.New(), Exportable: true}, EncryptedDEK: "invalid"}
	repository := &secretExportTestStore{secret: secret}
	wrongBucket := secretExportTestRequest(t, repository, make([]byte, 32), uuid.New(), secret.ID, accesscontrol.PermissionCredentialsReveal, "POST", "reveal", "")
	corrupt := secretExportTestRequest(t, repository, make([]byte, 32), secret.BucketID, secret.ID, accesscontrol.PermissionCredentialsReveal, "POST", "reveal", "")
	// Exact bucket lookup and bounded crypto failure are different from successful empty credentials.
	if wrongBucket.Code != 404 || corrupt.Code != 503 {
		t.Fatalf("statuses=%d/%d", wrongBucket.Code, corrupt.Code)
	}
}

// TestSecretExportPolicyRequiresExplicitBoolean rejects ambiguous policy bodies before any write.
func TestSecretExportPolicyRequiresExplicitBoolean(t *testing.T) {
	for _, body := range []string{`{}`, `{"exportable":null}`, `{"exportable":"true"}`, `{"exportable":true,"value":"secret"}`, `{"exportable":true} {}`, `{"exportable":true}` + strings.Repeat(" ", 1024) + `{}`} {
		_, err := decodeSecretExportPolicy(httptest.NewRequest("PUT", "/", strings.NewReader(body)))
		// Every invalid representation must be rejected instead of becoming a default false value.
		if err == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

// TestSecretExportabilityWriteBoundary excludes paired and connected-auth material from both write APIs.
func TestSecretExportabilityWriteBoundary(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	for _, credentialType := range []string{"bearer", "oauth", "oidc", "basic", "mtls"} {
		payload := SecretUpsertPayload{ServiceID: fixture.serviceID, BucketID: fixture.otherBucketID.String(), KeyName: "key", CredentialType: credentialType, Value: "synthetic", Exportable: true}
		body, _ := json.Marshal(payload)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("PUT", "/workspace/secrets", strings.NewReader(string(body))))
		// Exportability must not silently widen to any other credential family.
		if response.Code != 400 {
			t.Fatalf("type %s status=%d", credentialType, response.Code)
		}
	}
	// Generic secrets support explicit opt-in while omission remains private.
	for _, enabled := range []bool{false, true} {
		body, _ := json.Marshal(BucketSecretUpsertPayload{KeyName: "generic", Value: "synthetic", Exportable: enabled})
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("PUT", "/workspace/buckets/"+fixture.otherBucketID.String()+"/secrets", strings.NewReader(string(body))))
		// The stored flag must exactly match the supplied opt-in on every replacement.
		if response.Code != 204 || fixture.store.upsertedSecret.Exportable != enabled {
			t.Fatal("generic export policy not persisted")
		}
	}
}

// assertSecretRevealRevocation proves separate policy authority and the first read after revocation.
func assertSecretRevealRevocation(t *testing.T, repository *secretExportTestStore, fixture secretsFixture, secret store.WorkspaceSecret) {
	t.Helper()
	denied := secretExportTestRequest(t, repository, fixture.masterKey, secret.BucketID, secret.ID, accesscontrol.PermissionCredentialsReveal, "PUT", "exportable", `{"exportable":false}`)
	// Retrieval authority cannot promote itself into policy management.
	if denied.Code != 403 || repository.writes != 0 {
		t.Fatal("reveal grant changed export policy")
	}
	revoked := secretExportTestRequest(t, repository, fixture.masterKey, secret.BucketID, secret.ID, accesscontrol.PermissionCredentialsManage, "PUT", "exportable", `{"exportable":false}`)
	response := secretExportTestRequest(t, repository, fixture.masterKey, secret.BucketID, secret.ID, accesscontrol.PermissionCredentialsReveal, "POST", "reveal", "")
	// Disabling retrieval must affect the next call without waiting for runtime cache expiry.
	if revoked.Code != 204 || response.Code != 404 || strings.Contains(response.Body.String(), "synthetic-api-key") {
		t.Fatal("revoked policy still disclosed material")
	}
}

// TestSecretExportAPIKeySpellings verifies API opt-in for both historical UI and canonical CLI representations.
func TestSecretExportAPIKeySpellings(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	for _, kind := range []string{"apiKey", "api_key"} {
		payload := SecretUpsertPayload{ServiceID: fixture.serviceID, BucketID: fixture.otherBucketID.String(), KeyName: "key", CredentialType: kind, Value: "synthetic", Exportable: true}
		body, _ := json.Marshal(payload)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest("PUT", "/workspace/secrets", strings.NewReader(string(body))))
		// Every supported spelling normalizes to the same persisted exportable API-key family.
		if response.Code != 204 || fixture.store.upsertedSecret == nil || !fixture.store.upsertedSecret.Exportable || fixture.store.upsertedSecret.CredentialType != "api_key" {
			t.Fatalf("API key %s status=%d", kind, response.Code)
		}
	}
}
