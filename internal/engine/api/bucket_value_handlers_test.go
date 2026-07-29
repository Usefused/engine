package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

func TestUpsertBucketValueHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newBucketValueFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"key_name":"region",
		"location":"header",
		"value":"us-east-1"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+uuid.NewString()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown bucket, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedValue != nil {
		t.Fatal("value must not be persisted when bucket ownership check fails")
	}
}

func TestUpsertBucketValueHandler_Success(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"service_id":"` + fixture.serviceID.String() + `",
		"key_name":"region",
		"location":"header",
		"value":"us-east-1"
	}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.upsertedValue == nil || fixture.store.upsertedValue.BucketID != fixture.bucketID {
		t.Fatalf("expected value persisted for bucket %s, got %+v", fixture.bucketID, fixture.store.upsertedValue)
	}
}

// TestUpsertBucketValueHandlerRejectsHostOverride ensures the legacy value API
// cannot bypass connection-profile host allowlisting.
func TestUpsertBucketValueHandlerRejectsHostOverride(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	body := bytes.NewReader([]byte(`{"service_id":"` + fixture.serviceID.String() + `","key_name":"","location":"base_url","value":"https://attacker.example"}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || fixture.store.upsertedValue != nil {
		t.Fatalf("expected rejected host override, status=%d value=%+v", rr.Code, fixture.store.upsertedValue)
	}
}

// TestUpsertBucketValueHandlerRejectsProtectedHeader keeps static bucket values
// from replacing credentials or transport-owned headers.
func TestUpsertBucketValueHandlerRejectsProtectedHeader(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	body := bytes.NewReader([]byte(`{"service_id":"` + fixture.serviceID.String() + `","key_name":"Authorization","location":"header","value":"Bearer unsafe"}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || fixture.store.upsertedValue != nil {
		t.Fatalf("expected rejected protected header, status=%d value=%+v", rr.Code, fixture.store.upsertedValue)
	}
}

// TestDeleteBucketValueHandler_UnknownBucketReturnsNotFound also covers the
// fix that made this handler resolve workspaceID at all -- previously it
// discarded the account ID and never checked bucket ownership.
func TestDeleteBucketValueHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newBucketValueFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/values?bucket_id="+uuid.NewString()+"&service_id="+fixture.serviceID.String()+"&key_name=region", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown bucket, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedBucketID != uuid.Nil {
		t.Fatal("value must not be deleted when bucket ownership check fails")
	}
}

func TestDeleteBucketValueHandler_Success(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/values?bucket_id="+fixture.bucketID.String()+"&service_id="+fixture.serviceID.String()+"&key_name=region", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedBucketID != fixture.bucketID {
		t.Fatalf("expected delete to target bucket %s, got %s", fixture.bucketID, fixture.store.deletedBucketID)
	}
}

type bucketValueFixture struct {
	store     *bucketValueMockStore
	masterKey []byte
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

func newBucketValueFixture() bucketValueFixture {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	return bucketValueFixture{
		masterKey: []byte("12345678901234567890123456789012"),
		bucketID:  bucketID,
		serviceID: serviceID,
		store: &bucketValueMockStore{
			accountID:   uuid.New(),
			workspaceID: workspaceID,
			bucketID:    bucketID,
		},
	}
}

type bucketValueMockStore struct {
	store.Store
	accountID       uuid.UUID
	workspaceID     uuid.UUID
	bucketID        uuid.UUID
	bucketErr       error
	upsertErr       error
	deleteErr       error
	upsertedValue   *store.BucketValue
	deletedBucketID uuid.UUID
}

func (s *bucketValueMockStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *bucketValueMockStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

func (s *bucketValueMockStore) GetBucket(_ context.Context, bucketID uuid.UUID) (*store.Bucket, error) {
	if s.bucketErr != nil {
		return nil, s.bucketErr
	}
	return &store.Bucket{ID: bucketID}, nil
}

func (s *bucketValueMockStore) UpsertBucketValue(_ context.Context, val store.BucketValue) error {
	if s.upsertErr != nil {
		return s.upsertErr
	}
	saved := val
	s.upsertedValue = &saved
	return nil
}

func (s *bucketValueMockStore) DeleteBucketValue(_ context.Context, bucketID, _ uuid.UUID, _ string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	s.deletedBucketID = bucketID
	return nil
}
