package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type trafficControlFixture struct {
	*unifiedAppBundleControlStore
	target   uuid.UUID
	expected uuid.UUID
	promoted uuid.UUID
	failure  error
}

// UnifiedAppTrafficTarget projects the stored pointer without deriving it from the selected version.
func (f *trafficControlFixture) UnifiedAppTrafficTarget(context.Context, uuid.UUID) (uuid.UUID, error) {
	return f.target, nil
}

// PromoteUnifiedAppVersion records admitted arguments and simulates a transactional conflict.
func (f *trafficControlFixture) PromoteUnifiedAppVersion(_ context.Context, family, app, expected uuid.UUID) error {
	f.expected, f.promoted = expected, app
	return f.failure
}

// TestUnifiedAppTrafficControl verifies the public mutation boundary and explicit expected-target contract.
func TestUnifiedAppTrafficControl(t *testing.T) {
	cases := []struct {
		name, body string
		failure    error
		want       int
		writes     bool
	}{
		{"promote", `{"expected_active_app_id":"11111111-1111-4111-8111-111111111111"}`, nil, 200, true},
		{"empty target", `{"expected_active_app_id":""}`, nil, 200, true},
		{"missing target", `{}`, nil, 400, false},
		{"invalid target", `{"expected_active_app_id":"latest"}`, nil, 400, false},
		{"unknown field", `{"expected_active_app_id":"","force":true}`, nil, 400, false},
		{"trailing document", `{"expected_active_app_id":""}{}`, nil, 400, false},
		{"stale target", `{"expected_active_app_id":""}`, store.ErrUnifiedAppTrafficChanged, 409, true},
		{"unready", `{"expected_active_app_id":""}`, store.ErrUnifiedAppBundleNotFound, 409, true},
	}
	for _, tc := range cases {
		// Each request gets fresh authorization and mutation state.
		t.Run(tc.name, func(t *testing.T) {
			base, _, _ := newUnifiedAppBundleControlFixture(t)
			f := &trafficControlFixture{unifiedAppBundleControlStore: base, failure: tc.failure}
			router := newControlTestRouter(base.app.AccountID)
			router.Post("/apps/{app_id}/promote", UnifiedAppTrafficHandler(f))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/apps/"+base.app.AppID.String()+"/promote", strings.NewReader(tc.body)))
			// Validation failures must not reach persistence; successful receipts name the selected exact version.
			if response.Code != tc.want || (f.promoted != uuid.Nil) != tc.writes {
				t.Fatalf("status=%d promoted=%s body=%s", response.Code, f.promoted, response.Body.String())
			}
			if tc.want == 200 && !strings.Contains(response.Body.String(), base.app.AppID.String()) {
				t.Fatal("missing promoted target")
			}
		})
	}
}

// TestUnifiedAppTrafficAuthorization rejects unprivileged and foreign actors before mutation.
func TestUnifiedAppTrafficAuthorization(t *testing.T) {
	base, _, _ := newUnifiedAppBundleControlFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		// Both discovery and mutation require family access in the current workspace.
		t.Run(method, func(t *testing.T) {
			f := &trafficControlFixture{unifiedAppBundleControlStore: base}
			router := chi.NewRouter()
			router.Method(method, "/apps/{app_id}/traffic", UnifiedAppTrafficHandler(f))
			actor := accesscontrol.Actor{AccountID: base.app.AccountID, SubjectID: uuid.New(), WorkspaceID: uuid.New()}
			request := httptest.NewRequest(method, "/apps/"+base.app.AppID.String()+"/traffic", strings.NewReader(`{"expected_active_app_id":""}`))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// Authentication without grants may not read or change the deployment pointer.
			if response.Code != http.StatusForbidden || f.promoted != uuid.Nil {
				t.Fatalf("status=%d target=%s", response.Code, f.promoted)
			}
		})
	}
}

// TestUnifiedAppTrafficTypedGrants proves a family manager can promote while readers and other adapter grants cannot.
func TestUnifiedAppTrafficTypedGrants(t *testing.T) {
	cases := []struct {
		name, method string
		permission   accesscontrol.Permission
		want         int
	}{
		{"manager", http.MethodPost, accesscontrol.PermissionAppUnifiedAppManage, 200},
		{"reader discovery", http.MethodGet, accesscontrol.PermissionAppUnifiedAppRead, 200},
		{"reader mutation", http.MethodPost, accesscontrol.PermissionAppUnifiedAppRead, 403},
		{"SDK manager", http.MethodPost, accesscontrol.PermissionAppSDKManage, 403},
	}
	for _, tc := range cases {
		// Each test grants only the declared permission on this exact family.
		t.Run(tc.name, func(t *testing.T) {
			base, _, _ := newUnifiedAppBundleControlFixture(t)
			f := &trafficControlFixture{unifiedAppBundleControlStore: base}
			snapshot, err := accesscontrol.NewAuthorizationSnapshot(1, accesscontrol.Grant{Permission: tc.permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: base.app.AppFamilyID}})
			// A malformed fixture must not be mistaken for a permission denial.
			if err != nil {
				t.Fatal(err)
			}
			actor := accesscontrol.Actor{AccountID: base.app.AccountID, SubjectID: uuid.New(), WorkspaceID: uuid.New(), Authorization: snapshot}
			request := httptest.NewRequest(tc.method, "/apps/"+base.app.AppID.String()+"/traffic", strings.NewReader(`{"expected_active_app_id":""}`))
			request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
			router := chi.NewRouter()
			router.Method(tc.method, "/apps/{app_id}/traffic", UnifiedAppTrafficHandler(f))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			// Typed grants must match the exact Unified App action even without an owner bypass.
			if response.Code != tc.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			// Discovery and denial must both remain free of writes.
			if (tc.method == http.MethodGet || tc.want == 403) && f.promoted != uuid.Nil {
				t.Fatal("unexpected traffic mutation")
			}
		})
	}
}
