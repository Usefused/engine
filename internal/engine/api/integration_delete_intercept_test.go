package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

// deleteInterceptMockStore records calls relevant to the cleanup intercept.
// Embeds store.Store so unrelated methods don't need stubs.
type deleteInterceptMockStore struct {
	store.Store
	accountID uuid.UUID
	wsErr     error

	removeCalls      int
	verifyOwnerCalls int
	lastServiceID    uuid.UUID
	removeErr        error
}

func (m *deleteInterceptMockStore) GetAccountByAPIKey(_ context.Context, _ string) (uuid.UUID, error) {
	return m.accountID, nil
}

func (m *deleteInterceptMockStore) VerifyWorkspaceOwner(_ context.Context, _ uuid.UUID) error {
	m.verifyOwnerCalls++
	return m.wsErr
}

func (m *deleteInterceptMockStore) RemoveWorkspaceService(_ context.Context, serviceID uuid.UUID) error {
	m.removeCalls++
	m.lastServiceID = serviceID
	return m.removeErr
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

func (m *mockDeleteForwarder) ForwardAndInspect(w http.ResponseWriter, _ *http.Request, _ string, fn func([]byte)) {
	w.WriteHeader(m.status)
	fn(nil)
}

func TestDeleteIntercept_CleanupCalledOnSuccess(t *testing.T) {
	svcID := uuid.New()
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNoContent}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+svcID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	if s.removeCalls != 1 {
		t.Fatalf("expected RemoveWorkspaceService to be called once, got %d", s.removeCalls)
	}
	if s.lastServiceID != svcID {
		t.Errorf("expected serviceID %s, got %s", svcID, s.lastServiceID)
	}
}

func TestDeleteIntercept_NoCleanupOnRegistryFailure(t *testing.T) {
	// When the Registry returns 404 (service didn't exist), the local workspace
	// must not be touched -- the caller is handling a stale ID.
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNotFound}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+uuid.New().String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	if s.removeCalls != 0 {
		t.Errorf("expected no RemoveWorkspaceService call on 404, got %d", s.removeCalls)
	}
}

func TestDeleteIntercept_VerifiesWorkspaceOwnerBeforeCleanup(t *testing.T) {
	// Regression guard: workspace ownership must actually be checked
	// (VerifyWorkspaceOwner called) before RemoveWorkspaceService runs, not
	// skipped entirely.
	svcID := uuid.New()
	s := &deleteInterceptMockStore{
		accountID: uuid.New(),
	}
	fwd := &mockDeleteForwarder{status: http.StatusNoContent}

	req := httptest.NewRequest(http.MethodDelete, "/integrations/"+svcID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_valid")
	rec := httptest.NewRecorder()

	forwardIntegrationDeleteWithWorkspaceCleanup(fwd, s, rec, req, s.accountID)

	if s.verifyOwnerCalls != 1 {
		t.Errorf("expected VerifyWorkspaceOwner called once, got %d", s.verifyOwnerCalls)
	}
	if s.removeCalls != 1 {
		t.Errorf("expected RemoveWorkspaceService called once, got %d", s.removeCalls)
	}
}
