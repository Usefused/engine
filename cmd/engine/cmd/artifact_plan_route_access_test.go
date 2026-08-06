package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/store"
)

type artifactPlanRouteActor struct {
	name        string
	actor       accesscontrol.Actor
	allowCreate bool
	allowUpdate bool
}

func TestArtifactPlanRouteCreateUpdateRoleAndShareMatrix(t *testing.T) {
	workspaceID, serviceID, bucketID, artifactID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	actors := artifactPlanRouteActors(t, workspaceID, serviceID, bucketID, artifactID)
	for _, path := range []string{"/sdk-config/plan", "/mcp-config/plan", "/webhook-config/plan"} {
		for _, operation := range artifactPlanRouteOperations(artifactID) {
			for _, actor := range actors {
				t.Run(path+"/"+operation.name+"/"+actor.name, func(t *testing.T) {
					assertArtifactPlanRouteDecision(t, path, serviceID, bucketID, operation.state, actor.actor, operation.wantAccess(actor))
				})
			}
		}
	}
}

func TestArtifactPlanRouteIgnoresForgedClientArtifactIdentity(t *testing.T) {
	workspaceID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New()
	storedArtifactID, forgedArtifactID := uuid.New(), uuid.New()
	actor := artifactPlanSharedActor(t, workspaceID, serviceID, bucketID, forgedArtifactID, true)
	state := &store.ConfigState{LatestResourceID: &storedArtifactID}
	body := `{"config_key":"sdk:acceptance:1.0.0","artifact_id":"` + forgedArtifactID.String() + `","config":{"bucket":"default","services":{"` + acceptanceServiceName + `":{"version":"1.0.0"}}}}`
	status, calls := serveArtifactPlanRoute(t, "/sdk-config/plan", serviceID, bucketID, state, actor, body)
	assertArtifactPlanHTTPDecision(t, status, calls, false)
}

func TestArtifactApplyMiddlewareRevisionReplacementStopsBeforeRegistry(t *testing.T) {
	workspaceID, serviceID, bucketID, planID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	plan := &store.ConfigPlan{
		ID: planID, ConfigKey: "sdk:revision:1.0.0", ConfigType: store.ConfigTypeSDK,
		Status: store.ConfigPlanStatusPending, SourceHash: "source-hash", Revision: 1,
		ResolvedPayload: []byte(`{"bucket_id":"` + bucketID.String() + `","selections":[{"service_id":"` + serviceID.String() + `"}]}`),
		DesiredState:    []byte(`{"bucket":"default"}`),
	}
	configStore := &revisionReplacingConfigStore{plan: plan}
	stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "default"}}}
	resolver := newControlRequirementResolver(stores, configStore)
	registry := &revisionReplacementForwarder{}
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(api.SDKConfigApplyHandler(configStore, nil, registry))
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
	)
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/apply", strings.NewReader(`{"plan_id":"`+planID.String()+`","source_hash":"source-hash"}`))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "plan_revision_changed") {
		t.Fatalf("revision replacement response = %d %s", response.Code, response.Body.String())
	}
	if configStore.callCount() != 2 || registry.callCount() != 0 {
		t.Fatalf("plan/Registry calls = %d/%d, want 2/0", configStore.callCount(), registry.callCount())
	}
}

type revisionReplacingConfigStore struct {
	store.ConfigRepository
	mu    sync.Mutex
	plan  *store.ConfigPlan
	calls int
}

func (s *revisionReplacingConfigStore) GetConfigPlan(context.Context, uuid.UUID) (*store.ConfigPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	copy := *s.plan
	if s.calls > 1 {
		copy.Revision++
	}
	return &copy, nil
}

func (s *revisionReplacingConfigStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

type revisionReplacementForwarder struct {
	mu    sync.Mutex
	calls int
}

func (f *revisionReplacementForwarder) Forward(http.ResponseWriter, *http.Request, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
}

func (f *revisionReplacementForwarder) ForwardAndInspect(w http.ResponseWriter, request *http.Request, prefix string, _ func([]byte)) {
	f.Forward(w, request, prefix)
}

func (f *revisionReplacementForwarder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type artifactPlanRouteOperation struct {
	name       string
	state      *store.ConfigState
	wantAccess func(artifactPlanRouteActor) bool
}

func artifactPlanRouteOperations(artifactID uuid.UUID) []artifactPlanRouteOperation {
	return []artifactPlanRouteOperation{
		{name: "create", wantAccess: func(actor artifactPlanRouteActor) bool { return actor.allowCreate }},
		{name: "update", state: &store.ConfigState{LatestResourceID: &artifactID}, wantAccess: func(actor artifactPlanRouteActor) bool { return actor.allowUpdate }},
	}
}

func artifactPlanRouteActors(t *testing.T, workspaceID, serviceID, bucketID, artifactID uuid.UUID) []artifactPlanRouteActor {
	t.Helper()
	return []artifactPlanRouteActor{
		{name: accesscontrol.RoleOwner, actor: artifactPlanBuiltInActor(t, workspaceID, serviceID, bucketID, accesscontrol.RoleOwner), allowCreate: true, allowUpdate: true},
		{name: accesscontrol.RoleAdmin, actor: artifactPlanBuiltInActor(t, workspaceID, serviceID, bucketID, accesscontrol.RoleAdmin), allowCreate: true, allowUpdate: true},
		{name: accesscontrol.RoleBuilder, actor: artifactPlanBuiltInActor(t, workspaceID, serviceID, bucketID, accesscontrol.RoleBuilder), allowCreate: true},
		{name: accesscontrol.RoleViewer, actor: artifactPlanBuiltInActor(t, workspaceID, serviceID, bucketID, accesscontrol.RoleViewer)},
		{name: "shared-reader", actor: artifactPlanSharedActor(t, workspaceID, serviceID, bucketID, artifactID, false)},
		{name: "shared-manager", actor: artifactPlanSharedActor(t, workspaceID, serviceID, bucketID, artifactID, true), allowUpdate: true},
	}
}

func artifactPlanBuiltInActor(t *testing.T, workspaceID, serviceID, bucketID uuid.UUID, roleSlug string) accesscontrol.Actor {
	t.Helper()
	grants := artifactPlanSelectionGrants(serviceID, bucketID)
	for _, role := range accesscontrol.BuiltInRoles() {
		if role.Slug != roleSlug {
			continue
		}
		for _, permission := range role.Permissions {
			grants = append(grants, accesscontrol.Grant{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}})
		}
		return actorWithGrants(t, workspaceID, grants...)
	}
	t.Fatalf("built-in role %q not found", roleSlug)
	return accesscontrol.Actor{}
}

func artifactPlanSharedActor(t *testing.T, workspaceID, serviceID, bucketID, artifactID uuid.UUID, manager bool) accesscontrol.Actor {
	t.Helper()
	grants := artifactPlanSelectionGrants(serviceID, bucketID)
	grants = append(grants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAppRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: artifactID},
	})
	if manager {
		grants = append(grants, accesscontrol.Grant{
			Permission: accesscontrol.PermissionAppManage,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: artifactID},
		})
	}
	return actorWithGrants(t, workspaceID, grants...)
}

func artifactPlanSelectionGrants(serviceID, bucketID uuid.UUID) []accesscontrol.Grant {
	return []accesscontrol.Grant{
		{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}},
	}
}

func assertArtifactPlanRouteDecision(t *testing.T, path string, serviceID, bucketID uuid.UUID, state *store.ConfigState, actor accesscontrol.Actor, allowed bool) {
	t.Helper()
	body := `{"config_key":"sdk:acceptance:1.0.0","config":{"bucket":"default","services":{"` + acceptanceServiceName + `":{"version":"1.0.0"}}}}`
	status, downstreamCalls := serveArtifactPlanRoute(t, path, serviceID, bucketID, state, actor, body)
	assertArtifactPlanHTTPDecision(t, status, downstreamCalls, allowed)
}

func serveArtifactPlanRoute(t *testing.T, path string, serviceID, bucketID uuid.UUID, state *store.ConfigState, actor accesscontrol.Actor, body string) (int, int) {
	t.Helper()
	stores := &controlRequirementStoreStub{
		services: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: acceptanceServiceName}},
		buckets:  []store.Bucket{{ID: bucketID, Name: "default"}},
	}
	if state != nil && state.LatestResourceID != nil {
		stores.apps = map[uuid.UUID]store.App{
			*state.LatestResourceID: {AppID: *state.LatestResourceID, AppFamilyID: *state.LatestResourceID},
		}
	}
	resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{state: state})
	downstreamCalls := 0
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, downstreamCalls
}

func assertArtifactPlanHTTPDecision(t *testing.T, status, downstreamCalls int, allowed bool) {
	t.Helper()
	if allowed && (status != http.StatusNoContent || downstreamCalls != 1) {
		t.Fatalf("allowed status/calls = %d/%d, want 204/1", status, downstreamCalls)
	}
	if !allowed && (status != http.StatusForbidden || downstreamCalls != 0) {
		t.Fatalf("denied status/calls = %d/%d, want 403/0", status, downstreamCalls)
	}
}
