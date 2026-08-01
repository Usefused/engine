package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

func TestUpsertSecretHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newSecretsFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"bucket_id":"` + uuid.NewString() + `",
		"key_name":"Authorization",
		"credential_type":"bearer",
		"value":"secret-token"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown bucket, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedSecret != nil {
		t.Fatal("secret must not be persisted when bucket ownership check fails")
	}
}

func TestUpsertSecretHandler_DefaultBucketWhenOmitted(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"key_name":"Authorization",
		"credential_type":"bearer",
		"value":"secret-token"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedSecret == nil {
		t.Fatal("expected secret to be persisted")
	}
	if fixture.store.upsertedSecret.BucketID != fixture.defaultBucketID {
		t.Fatalf("expected secret to land in default bucket %s, got %s", fixture.defaultBucketID, fixture.store.upsertedSecret.BucketID)
	}
}

func TestUpsertSecretHandler_RejectsSingleMTLSSecret(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"key_name":"mtls_cert",
		"credential_type":"mtls",
		"value":"cert"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for single mTLS secret, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedSecret != nil {
		t.Fatal("single mTLS material must not be persisted")
	}
}

func TestUpsertSecretHandler_RejectsSingleBasicSecret(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"key_name":"basicAuth_username",
		"credential_type":"basic",
		"value":"user"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for single basic secret, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedSecret != nil {
		t.Fatal("single basic material must not be persisted")
	}
}

func TestUpsertSecretsHandler_StoresMTLSPairAtomically(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	cert, key := workspaceTestMTLSPair(t)

	body := bytes.NewReader([]byte(`{
		"bucket_id":"` + fixture.otherBucketID.String() + `",
		"secrets":[
			{"service_id":"` + fixture.serviceID.String() + `","key_name":"clientCert_cert","credential_type":"mtls","value":` + strconv.Quote(cert) + `},
			{"service_id":"` + fixture.serviceID.String() + `","key_name":"clientCert_key","credential_type":"mtls","value":` + strconv.Quote(key) + `}
		]
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets/bulk", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("bulk upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.upsertedSecrets) != 2 {
		t.Fatalf("expected two atomic secret writes, got %d", len(fixture.store.upsertedSecrets))
	}
	if fixture.store.upsertedSecrets[0].BucketID != fixture.otherBucketID {
		t.Fatalf("expected explicit bucket %s, got %s", fixture.otherBucketID, fixture.store.upsertedSecrets[0].BucketID)
	}
}

func TestUpsertSecretsHandler_RejectsIncompleteBasicPair(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"secrets":[
			{"service_id":"` + fixture.serviceID.String() + `","key_name":"basicAuth_username","credential_type":"basic","value":"user"}
		]
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets/bulk", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for incomplete basic pair, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.upsertedSecrets) != 0 {
		t.Fatal("incomplete basic material must not be persisted")
	}
}

func TestUpsertSecretsHandler_RejectsInvalidMTLSPair(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"secrets":[
			{"service_id":"` + fixture.serviceID.String() + `","key_name":"clientCert_cert","credential_type":"mtls","value":"not-a-cert"},
			{"service_id":"` + fixture.serviceID.String() + `","key_name":"clientCert_key","credential_type":"mtls","value":"not-a-key"}
		]
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/secrets/bulk", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid mTLS pair, got %d body=%s", rr.Code, rr.Body.String())
	}
	if len(fixture.store.upsertedSecrets) != 0 {
		t.Fatal("invalid mTLS material must not be persisted")
	}
}

func TestDeleteSecretHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newSecretsFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/secrets?service_id="+fixture.serviceID.String()+"&key_name=Authorization&bucket_id="+uuid.NewString(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown bucket, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedBucketID != uuid.Nil {
		t.Fatal("secret must not be deleted when bucket ownership check fails")
	}
}

// TestDeleteSecretHandler_DefaultBucketWhenOmitted is the regression test for
// the fixed bug: omitting bucket_id used to fall back to workspaceID (never a
// real bucket id), silently deleting nothing. It must now resolve the same
// default bucket UpsertSecretHandler would use.
func TestDeleteSecretHandler_DefaultBucketWhenOmitted(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/secrets?service_id="+fixture.serviceID.String()+"&key_name=Authorization", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedBucketID != fixture.defaultBucketID {
		t.Fatalf("expected delete to target default bucket %s, got %s", fixture.defaultBucketID, fixture.store.deletedBucketID)
	}
}

func TestDeleteSecretHandler_ExplicitBucketID(t *testing.T) {
	fixture := newSecretsFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/secrets?service_id="+fixture.serviceID.String()+"&key_name=Authorization&bucket_id="+fixture.otherBucketID.String(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedBucketID != fixture.otherBucketID {
		t.Fatalf("expected delete to target explicit bucket %s, got %s", fixture.otherBucketID, fixture.store.deletedBucketID)
	}
}

type secretsFixture struct {
	store           *secretsMockStore
	masterKey       []byte
	serviceID       uuid.UUID
	defaultBucketID uuid.UUID
	otherBucketID   uuid.UUID
}

func newSecretsFixture() secretsFixture {
	workspaceID := uuid.New()
	defaultBucketID := uuid.New()
	otherBucketID := uuid.New()
	serviceID := uuid.New()
	return secretsFixture{
		masterKey:       []byte("12345678901234567890123456789012"),
		serviceID:       serviceID,
		defaultBucketID: defaultBucketID,
		otherBucketID:   otherBucketID,
		store: &secretsMockStore{
			accountID:       uuid.New(),
			workspaceID:     workspaceID,
			defaultBucketID: defaultBucketID,
			otherBucketID:   otherBucketID,
		},
	}
}

type secretsMockStore struct {
	store.Store
	accountID       uuid.UUID
	workspaceID     uuid.UUID
	defaultBucketID uuid.UUID
	otherBucketID   uuid.UUID
	bucketErr       error
	upsertErr       error
	deleteErr       error
	upsertedSecret  *store.WorkspaceSecret
	upsertedSecrets []store.WorkspaceSecret
	deletedBucketID uuid.UUID
}

func (s *secretsMockStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *secretsMockStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

func (s *secretsMockStore) GetBucket(_ context.Context, bucketID uuid.UUID) (*store.Bucket, error) {
	if s.bucketErr != nil {
		return nil, s.bucketErr
	}
	return &store.Bucket{ID: bucketID}, nil
}

func (s *secretsMockStore) ListBuckets(context.Context) ([]store.Bucket, error) {
	return []store.Bucket{
		{ID: s.defaultBucketID, Name: "default", IsDefault: true},
		{ID: s.otherBucketID, Name: "other"},
	}, nil
}

func (s *secretsMockStore) UpsertSecret(_ context.Context, secret store.WorkspaceSecret) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	saved := secret
	s.upsertedSecret = &saved
	return nil
}

func (s *secretsMockStore) UpsertSecrets(_ context.Context, secrets []store.WorkspaceSecret) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upsertedSecrets = append(s.upsertedSecrets, secrets...)
	return nil
}

func (s *secretsMockStore) DeleteSecret(_ context.Context, bucketID, _ uuid.UUID, _ string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletedBucketID = bucketID
	return nil
}
