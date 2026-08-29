package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/webhookstream"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type appLifecycleTestStore struct {
	store.Store
	apps          map[uuid.UUID]store.App
	deprecated    uuid.UUID
	undeprecated  uuid.UUID
	deactivated   uuid.UUID
	deactivatedBy uuid.UUID
	message       string
	plannedAt     *time.Time
}

func (s *appLifecycleTestStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	app, ok := s.apps[appID]
	if !ok {
		return nil, store.ErrAppNotFound
	}
	return &app, nil
}

func (s *appLifecycleTestStore) DeprecateApp(_ context.Context, appID uuid.UUID, message string, plannedAt *time.Time) error {
	if _, ok := s.apps[appID]; !ok {
		return store.ErrAppNotFound
	}
	s.deprecated, s.message, s.plannedAt = appID, message, plannedAt
	return nil
}

func (s *appLifecycleTestStore) UndeprecateApp(_ context.Context, appID uuid.UUID) error {
	if _, ok := s.apps[appID]; !ok {
		return store.ErrAppNotFound
	}
	s.undeprecated = appID
	return nil
}

func (s *appLifecycleTestStore) DeactivateAppVersion(_ context.Context, appID, actorID uuid.UUID) error {
	if _, ok := s.apps[appID]; !ok {
		return store.ErrAppNotFound
	}
	s.deactivated, s.deactivatedBy = appID, actorID
	delete(s.apps, appID)
	return nil
}

func mountAppLifecycleRoutes(accountID uuid.UUID, s store.Store) chi.Router {
	router := newControlTestRouter(accountID)
	router.Post("/apps/{app_id}/deprecate", DeprecateAppHandler(s))
	router.Post("/apps/{app_id}/undeprecate", UndeprecateAppHandler(s))
	router.Delete("/apps/{app_id}/", DeactivateAppHandler(s, nil))
	return router
}

func TestDeprecateAppHandlerRecordsWarningAndDate(t *testing.T) {
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	appStore := &appLifecycleTestStore{apps: map[uuid.UUID]store.App{appID: {
		AppID: appID, AppFamilyID: familyID, AccountID: accountID, GeneratorVersion: "generator-v1",
	}}}
	router := mountAppLifecycleRoutes(accountID, appStore)
	request := httptest.NewRequest(http.MethodPost, "/apps/"+appID.String()+"/deprecate", strings.NewReader(`{"message":"Upgrade before removal","planned_deactivation_at":"2026-09-01T00:00:00Z"}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", response.Code, response.Body.String())
	}
	if appStore.deprecated != appID || appStore.message != "Upgrade before removal" || appStore.plannedAt == nil {
		t.Fatalf("deprecation was not persisted: %#v", appStore)
	}
	if !strings.Contains(response.Body.String(), `"app_family_id":"`+familyID.String()+`"`) {
		t.Fatalf("response missing family identity: %s", response.Body.String())
	}
}

func TestDeprecateAppHandlerRequiresUserFacingMessage(t *testing.T) {
	accountID, appID := uuid.New(), uuid.New()
	appStore := &appLifecycleTestStore{apps: map[uuid.UUID]store.App{appID: {AppID: appID, AccountID: accountID}}}
	request := httptest.NewRequest(http.MethodPost, "/apps/"+appID.String()+"/deprecate", strings.NewReader(`{"message":" "}`))
	response := httptest.NewRecorder()
	mountAppLifecycleRoutes(accountID, appStore).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || appStore.deprecated != uuid.Nil {
		t.Fatalf("status=%d deprecated=%s body=%s", response.Code, appStore.deprecated, response.Body.String())
	}
}

func TestUndeprecateAppHandlerDoesNotReactivateDeletedApp(t *testing.T) {
	accountID, appID := uuid.New(), uuid.New()
	appStore := &appLifecycleTestStore{apps: map[uuid.UUID]store.App{}}
	request := httptest.NewRequest(http.MethodPost, "/apps/"+appID.String()+"/undeprecate", nil)
	response := httptest.NewRecorder()
	mountAppLifecycleRoutes(accountID, appStore).ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || appStore.undeprecated != uuid.Nil {
		t.Fatalf("status=%d undeprecated=%s", response.Code, appStore.undeprecated)
	}
}

// TestDeactivateAppHandlerHardDeletesExactVersion proves HTTP deactivation also closes exact live receivers.
func TestDeactivateAppHandlerHardDeletesExactVersion(t *testing.T) {
	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	appStore := &appLifecycleTestStore{apps: map[uuid.UUID]store.App{appID: {
		AppID: appID, AppFamilyID: familyID, AccountID: accountID,
	}}}
	registry := webhookstream.NewRegistry()
	registration, ok := registry.Register(uuid.New(), appID)
	// The fixture represents a stream whose initial source revalidation completed before deactivation.
	if !ok || !registration.Confirm() {
		t.Fatal("confirm webhook stream registration")
	}
	cachedStore := store.NewCachedStoreWithAppRuntimeInvalidator(appStore, nil, registry)
	request := httptest.NewRequest(http.MethodDelete, "/apps/"+appID.String()+"/", nil)
	response := httptest.NewRecorder()
	mountAppLifecycleRoutes(accountID, cachedStore).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if appStore.deactivated != appID || appStore.deactivatedBy == uuid.Nil {
		t.Fatalf("deactivation=%s actor=%s", appStore.deactivated, appStore.deactivatedBy)
	}
	if _, err := appStore.GetApp(context.Background(), appID); !errors.Is(err, store.ErrAppNotFound) {
		t.Fatalf("deactivated app remains executable: %v", err)
	}
	select {
	case <-registration.Done():
		// Successful HTTP persistence reaches the shared CachedStore invalidation boundary.
	default:
		t.Fatal("HTTP deactivation left exact-app webhook stream active")
	}
}

func TestAppLifecycleRejectsCrossAccountTarget(t *testing.T) {
	accountID, appID := uuid.New(), uuid.New()
	appStore := &appLifecycleTestStore{apps: map[uuid.UUID]store.App{appID: {AppID: appID, AccountID: uuid.New()}}}
	request := httptest.NewRequest(http.MethodDelete, "/apps/"+appID.String()+"/", nil)
	response := httptest.NewRecorder()
	mountAppLifecycleRoutes(accountID, appStore).ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || appStore.deactivated != uuid.Nil {
		t.Fatalf("status=%d deactivated=%s", response.Code, appStore.deactivated)
	}
}
