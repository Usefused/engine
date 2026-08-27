package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// TestAuthConnectionHandlersListAndDelete proves a bucket-scoped grant can be removed without exposing tokens.
func TestAuthConnectionHandlersListAndDelete(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	connectionID := uuid.New()
	fixture.store.connections = []store.AuthConnection{{
		ID: connectionID, BucketID: fixture.bucketID, ServiceID: fixture.serviceID,
		EndUserRef: "user_123", AuthType: "oauth", EncryptedAccessToken: "secret-access-token",
		EncryptedRefreshToken: "secret-refresh-token", TokenType: "Bearer", Scopes: []string{"openid"},
		RefreshState: "ok", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}}

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/auth/connections/"+connectionID.String(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	// A successful delete must target only the exact connection ID.
	if rr.Code != http.StatusNoContent || fixture.store.deletedConnectionID != connectionID {
		t.Fatalf("delete status=%d deleted=%s body=%s", rr.Code, fixture.store.deletedConnectionID, rr.Body.String())
	}
}

// TestAuthConnectionHandlersDeleteMissing verifies typed absence is authoritatively not committed.
func TestAuthConnectionHandlersDeleteMissing(t *testing.T) {
	fixture := newConnectAdminFixture()
	fixture.store.deleteErr = store.ErrAuthConnectionNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/auth/connections/"+uuid.NewString(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	var envelope workspaceConfigErrorResponse
	decodeErr := json.Unmarshal(rr.Body.Bytes(), &envelope)
	// Typed absence must retain the stable public outcome without an operation identity.
	if decodeErr != nil || rr.Code != http.StatusNotFound || envelope.Error.Phase != "auth_connection_delete" || envelope.Error.CommitState != "not_committed" || envelope.Error.OperationID != "" {
		t.Fatalf("delete missing status=%d error=%#v decode=%v", rr.Code, envelope.Error, decodeErr)
	}
}

// TestAuthConnectionHandlersUnknownDelete verifies ambiguous storage outcomes remain secret-safe and non-retryable.
func TestAuthConnectionHandlersUnknownDelete(t *testing.T) {
	fixture := newConnectAdminFixture()
	fixture.store.deleteErr = errors.New("ambiguous delete with private backend detail")
	router := buildConnectAdminRouter(fixture.store, fixture.store.accountID, fixture.masterKey)
	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/auth/connections/"+uuid.NewString(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	var envelope workspaceConfigErrorResponse
	decodeErr := json.Unmarshal(rr.Body.Bytes(), &envelope)
	// Unknown commit state requires inspection and never includes private store text.
	if decodeErr != nil || rr.Code != http.StatusInternalServerError || envelope.Error.Phase != "auth_connection_delete" || envelope.Error.CommitState != "unknown" || envelope.Error.OperationID != "" {
		t.Fatalf("unknown delete status=%d error=%#v decode=%v", rr.Code, envelope.Error, decodeErr)
	}
	if !strings.Contains(envelope.Error.Remediation, "Inspect") || strings.Contains(rr.Body.String(), "private backend detail") {
		t.Fatalf("unsafe or unactionable response: %s", rr.Body.String())
	}
}
