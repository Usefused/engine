package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

// deleteInterceptMockStore records calls relevant to the cleanup intercept.
// Embeds store.Store so unrelated methods don't need stubs.
type deleteInterceptMockStore struct {
	store.Store
	store.ServiceLifecycleStore
	accountID uuid.UUID

	applyCalls    int
	lastServiceID uuid.UUID
	deleted       bool
	observedAt    time.Time
}

// ApplyServiceLifecycle records confirmed compaction, leaving membership-only removal unstubbed so accidental fallback fails.
func (m *deleteInterceptMockStore) ApplyServiceLifecycle(_ context.Context, serviceID uuid.UUID, _ string, _ *time.Time, deleted bool, observedAt time.Time) error {
	m.applyCalls++
	m.lastServiceID = serviceID
	m.deleted, m.observedAt = deleted, observedAt
	return nil
}

// ─── isIntegrationDeletePath ───────────────────────────────────────────────

func TestIsIntegrationDeletePath_TopLevel(t *testing.T) {
	svcID := uuid.New().String()
	if !isIntegrationDeletePath(http.MethodDelete, "/integrations/"+svcID) {
		t.Error("expected true for top-level service delete")
	}
}

func TestIsIntegrationDeletePath_Session_IsNotTopLevel(t *testing.T) {
	// DELETE /integrations/session/{id} must NOT trigger workspace cleanup --
	// it deletes an agent session, not a Registry service.
	if isIntegrationDeletePath(http.MethodDelete, "/integrations/session/abc123") {
		t.Error("expected false for session delete sub-resource")
	}
}

func TestIsIntegrationDeletePath_ImportWarnings_IsNotTopLevel(t *testing.T) {
	svcID := uuid.New().String()
	if isIntegrationDeletePath(http.MethodDelete, "/integrations/"+svcID+"/import-warnings") {
		t.Error("expected false for import-warnings sub-resource delete")
	}
}

func TestIsIntegrationDeletePath_Post_IsNotDelete(t *testing.T) {
	if isIntegrationDeletePath(http.MethodPost, "/integrations/"+uuid.New().String()) {
		t.Error("expected false for non-DELETE method")
	}
}

// ─── forwardIntegrationDeleteWithWorkspaceCleanup ──────────────────────────

// mockDeleteForwarder returns a configurable HTTP status, simulating the Registry response.
type mockDeleteForwarder struct {
	status int
}

func (m *mockDeleteForwarder) Forward(w http.ResponseWriter, _ *http.Request, _ string) {
	w.WriteHeader(m.status)
}

// ForwardAndInspect supplies the same status to the cleanup callback so these
// tests retain production's inspect-after-Registry contract.
func (m *mockDeleteForwarder) ForwardAndInspect(w http.ResponseWriter, _ *http.Request, _ string, fn func(*http.Response, []byte)) {
	w.WriteHeader(m.status)
	fn(&http.Response{StatusCode: m.status}, nil)
}

// TestDeleteIntercept_CleanupCalledOnSuccess requires durable compaction rather than the retired membership-only cleanup.
func TestDeleteIntercept_CleanupCalledOnSuccess(t *testing.T) {
	svcID := uuid.New()
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNoContent}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+svcID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	req = controlTestRequest(req, s.accountID)
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	// A confirmed Registry removal must produce one terminal lifecycle receipt.
	if s.applyCalls != 1 || !s.deleted || s.observedAt.IsZero() {
		t.Fatalf("expected one confirmed deletion receipt, got %+v", s)
	}
	// Cleanup must remain scoped to the exact deleted Registry identity.
	if s.lastServiceID != svcID {
		t.Errorf("expected serviceID %s, got %s", svcID, s.lastServiceID)
	}
}

// TestDeleteIntercept_NoCleanupOnRegistryFailure preserves local definitions until Registry confirms removal.
func TestDeleteIntercept_NoCleanupOnRegistryFailure(t *testing.T) {
	// When the Registry returns 404 (service didn't exist), the local workspace
	// must not be touched -- the caller is handling a stale ID.
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNotFound}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+uuid.New().String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	req = controlTestRequest(req, s.accountID)
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	// A missing definition response alone is not an authenticated deletion receipt.
	if s.applyCalls != 0 {
		t.Errorf("expected no lifecycle compaction on 404, got %d", s.applyCalls)
	}
}

// TestDeleteIntercept_RequiresAuthenticatedActorBeforeCleanup keeps deletion propagation inside the control authorization boundary.
func TestDeleteIntercept_RequiresAuthenticatedActorBeforeCleanup(t *testing.T) {
	svcID := uuid.New()
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNoContent}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+svcID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	// A credential string cannot replace the actor admitted by authentication middleware.
	if s.applyCalls != 0 {
		t.Errorf("expected no cleanup without an authenticated Actor, got %d", s.applyCalls)
	}
}
