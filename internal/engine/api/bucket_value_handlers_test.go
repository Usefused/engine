package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestUpsertBucketValueHandler_UnknownBucketReturnsNotFound verifies the CLI
// receives the stable structured absence diagnosis instead of plain text.
func TestUpsertBucketValueHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newBucketValueFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

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
	var envelope workspaceConfigErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "bucket_not_found" || envelope.Error.Remediation == "" {
		t.Fatalf("bucket error envelope = %#v, decode error=%v", envelope, err)
	}
	// Bucket admission failure proves no value write was attempted.
	if envelope.Error.Phase != "bucket_value_upsert" || envelope.Error.CommitState != "not_committed" || envelope.Error.OperationID != "" {
		t.Fatalf("unexpected mutation metadata: %#v", envelope.Error)
	}
	if fixture.store.upsertedValue != nil {
		t.Fatal("value must not be persisted when bucket ownership check fails")
	}
}

// TestUpsertBucketValueHandler_UnknownSaveOutcomeRequiresInspection verifies an
// unclassified repository error does not invite a blind retry.
func TestUpsertBucketValueHandler_UnknownSaveOutcomeRequiresInspection(t *testing.T) {
	fixture := newBucketValueFixture()
	fixture.store.upsertErr = errors.New("ambiguous commit: private database detail")
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	body := bytes.NewReader([]byte(`{"service_id":"` + fixture.serviceID.String() + `","key_name":"region","location":"header","value":"us-east-1"}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	var envelope workspaceConfigErrorResponse
	// The client receives certainty and remediation without the repository cause.
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if rr.Code != http.StatusInternalServerError || envelope.Error.Code != "bucket_value_save_failed" || envelope.Error.Phase != "bucket_value_upsert" || envelope.Error.CommitState != "unknown" || envelope.Error.OperationID != "" {
		t.Fatalf("unexpected error response: status=%d error=%#v", rr.Code, envelope.Error)
	}
	if !strings.Contains(envelope.Error.Remediation, "Inspect") || strings.Contains(rr.Body.String(), "private database detail") {
		t.Fatalf("unsafe or unactionable response: %s", rr.Body.String())
	}
}

func TestUpsertBucketValueHandler_Success(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

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
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
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
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	body := bytes.NewReader([]byte(`{"service_id":"` + fixture.serviceID.String() + `","key_name":"Authorization","location":"header","value":"Bearer unsafe"}`))
	req := httptest.NewRequest(http.MethodPut, "/workspace/buckets/"+fixture.bucketID.String()+"/values", body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || fixture.store.upsertedValue != nil {
		t.Fatalf("expected rejected protected header, status=%d value=%+v", rr.Code, fixture.store.upsertedValue)
	}
}

// TestDeleteBucketValueHandler_UnknownBucketReturnsNotFound verifies the path
// bucket is checked authoritatively before any deletion occurs.
func TestDeleteBucketValueHandler_UnknownBucketReturnsNotFound(t *testing.T) {
	fixture := newBucketValueFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/values?service_id="+fixture.serviceID.String()+"&key_name=region", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown bucket, got %d body=%s", rr.Code, rr.Body.String())
	}
	var envelope workspaceConfigErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "bucket_not_found" {
		t.Fatalf("bucket error envelope = %#v, decode error=%v", envelope, err)
	}
	if fixture.store.deletedBucketID != uuid.Nil {
		t.Fatal("value must not be deleted when bucket ownership check fails")
	}
}

// TestDeleteBucketValueHandler_Success proves the CLI route works without the
// obsolete redundant bucket_id query parameter.
func TestDeleteBucketValueHandler_Success(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/values?service_id="+fixture.serviceID.String()+"&key_name=region", nil)
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

// TestDeleteBucketValueHandlerUsesPathBucketID prevents a redundant query
// value from overriding the bucket identity authorized by routing middleware.
func TestDeleteBucketValueHandlerUsesPathBucketID(t *testing.T) {
	fixture := newBucketValueFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/values?bucket_id="+uuid.NewString()+"&service_id="+fixture.serviceID.String()+"&key_name=region", nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNoContent || fixture.store.deletedBucketID != fixture.bucketID {
		t.Fatalf("delete status=%d bucket=%s, want path bucket %s: %s", rr.Code, fixture.store.deletedBucketID, fixture.bucketID, rr.Body.String())
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
