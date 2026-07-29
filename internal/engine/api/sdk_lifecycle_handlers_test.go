package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

func mountSDKLifecycleRoutes(s store.Store) chi.Router {
	r := chi.NewRouter()
	r.Post("/sdk-config/{id}/activate", ActivateSDKHandler(s))
	r.Post("/sdk-config/{id}/deactivate", DeactivateSDKHandler(s))
	r.Delete("/sdk-config/{id}", DeleteSDKHandler(s))
	return r
}

func TestActivateSDKHandler_EmptyBodyReactivatesExistingScope(t *testing.T) {
	accountID := uuid.New()
	workspaceID := uuid.New()
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID:   accountID,
		workspaceID: workspaceID,
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: accountID, ArtifactID: artifactID, ScopeSchemaVersion: models.ArtifactScopeSchemaVersion},
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/activate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.reactivatedArtifactIDs) != 1 || s.reactivatedArtifactIDs[0] != artifactID {
		t.Fatalf("expected sdk %s to be reactivated, got %#v", artifactID, s.reactivatedArtifactIDs)
	}
	var resp sdkActivateResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.AuthToken != "" {
		t.Fatalf("reactivate-only shape must not mint a new token, got %q", resp.AuthToken)
	}
	if resp.MCPURL == "" {
		t.Fatal("expected a non-empty mcp_url")
	}
}

func TestActivateSDKHandler_EmptyBodyWithNoExistingScopeReturns404(t *testing.T) {
	s := &workspaceTestStore{accountID: uuid.New()}
	r := mountSDKLifecycleRoutes(s)

	artifactID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/activate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when nothing exists to reactivate, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.reactivatedArtifactIDs) != 0 {
		t.Fatalf("must not reactivate anything on a 404, got %#v", s.reactivatedArtifactIDs)
	}
}

func TestActivateSDKHandler_RejectsReactivateForAnotherAccountsScope(t *testing.T) {
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: uuid.New(), ArtifactID: artifactID}, // owned by someone else
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/activate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 owner mismatch, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.reactivatedArtifactIDs) != 0 {
		t.Fatalf("must not reactivate a scope owned by another account, got %#v", s.reactivatedArtifactIDs)
	}
}

func TestActivateSDKHandler_BodyCannotCreateScope(t *testing.T) {
	artifactID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New()}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/activate", bytes.NewBufferString(`{"selections":[]}`))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 because lifecycle activation cannot create a scope, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.savedScopes) != 0 {
		t.Fatalf("expected config apply to remain the only creation path, got %#v", s.savedScopes)
	}
}

func TestDeactivateSDKHandler_DeactivatesAndKillsSessions(t *testing.T) {
	accountID := uuid.New()
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: accountID, ArtifactID: artifactID},
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/deactivate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deactivatedArtifactIDs) != 1 || s.deactivatedArtifactIDs[0] != artifactID {
		t.Fatalf("expected sdk %s to be deactivated, got %#v", artifactID, s.deactivatedArtifactIDs)
	}
	if s.mockScopes[artifactID].DeactivatedAt == nil {
		t.Fatal("expected DeactivatedAt to be set on the underlying scope")
	}
}

func TestDeactivateSDKHandler_UnknownSDKReturns404(t *testing.T) {
	s := &workspaceTestStore{accountID: uuid.New()}
	r := mountSDKLifecycleRoutes(s)

	artifactID := uuid.New()
	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/deactivate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deactivatedArtifactIDs) != 0 {
		t.Fatalf("must not attempt to deactivate a scope that doesn't exist, got %#v", s.deactivatedArtifactIDs)
	}
}

func TestDeactivateSDKHandler_RejectsAnotherAccountsScope(t *testing.T) {
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(),
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: uuid.New(), ArtifactID: artifactID},
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/"+artifactID.String()+"/deactivate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deactivatedArtifactIDs) != 0 {
		t.Fatalf("must not deactivate a scope owned by another account, got %#v", s.deactivatedArtifactIDs)
	}
}

func TestDeleteSDKHandler_DeletesScope(t *testing.T) {
	accountID := uuid.New()
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID: accountID,
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: accountID, ArtifactID: artifactID},
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodDelete, "/sdk-config/"+artifactID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deletedScopes) != 1 || s.deletedScopes[0] != artifactID {
		t.Fatalf("expected sdk %s to be deleted, got %#v", artifactID, s.deletedScopes)
	}
	if _, stillExists := s.mockScopes[artifactID]; stillExists {
		t.Fatal("expected the scope to be gone after delete")
	}
}

func TestDeleteSDKHandler_UnknownSDKReturns404(t *testing.T) {
	s := &workspaceTestStore{accountID: uuid.New()}
	r := mountSDKLifecycleRoutes(s)

	artifactID := uuid.New()
	req := httptest.NewRequest(http.MethodDelete, "/sdk-config/"+artifactID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deletedScopes) != 0 {
		t.Fatalf("must not attempt to delete a scope that doesn't exist, got %#v", s.deletedScopes)
	}
}

func TestDeleteSDKHandler_RejectsAnotherAccountsScope(t *testing.T) {
	artifactID := uuid.New()
	s := &workspaceTestStore{
		accountID: uuid.New(),
		mockScopes: map[uuid.UUID]*store.ArtifactScope{
			artifactID: {AccountID: uuid.New(), ArtifactID: artifactID},
		},
	}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodDelete, "/sdk-config/"+artifactID.String(), nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.deletedScopes) != 0 {
		t.Fatalf("must not delete a scope owned by another account, got %#v", s.deletedScopes)
	}
}

func TestParseArtifactIDParam_RejectsInvalidUUID(t *testing.T) {
	s := &workspaceTestStore{accountID: uuid.New()}
	r := mountSDKLifecycleRoutes(s)

	req := httptest.NewRequest(http.MethodPost, "/sdk-config/not-a-uuid/activate", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed sdk id, got %d: %s", rr.Code, rr.Body.String())
	}
}
