package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/api"
	"github.com/Usefused/engine/internal/engine/store"
)

type controlRequirementStoreStub struct {
	store.Store
	bucket          *store.Bucket
	defaultID       uuid.UUID
	services        []store.WorkspaceService
	buckets         []store.Bucket
	bucketLoads     int
	defaultLoads    int
	serviceLoads    int
	batchBuckets    int
	displayNames    map[accesscontrol.ResourceRef]string
	displayLoads    int
	localServiceIDs map[string]uuid.UUID
	slugLoads       int
	apps            map[uuid.UUID]store.App
	appLoads        int
	families        map[uuid.UUID]store.AppFamily
}

func TestAppAccessRequirementsUseFamilyBoundary(t *testing.T) {
	accountID, appID, familyID := uuid.New(), uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{apps: map[uuid.UUID]store.App{
		appID: {AppID: appID, AppFamilyID: familyID, AccountID: accountID},
	}}
	resolver := newControlRequirementResolver(stores, nil)
	actor := accesscontrol.Actor{AccountID: accountID}

	tests := []struct {
		method     string
		path       string
		permission accesscontrol.Permission
	}{
		{method: http.MethodPost, path: "/apps/" + appID.String() + "/deprecate", permission: accesscontrol.PermissionAppManage},
		{method: http.MethodGet, path: "/sdks/" + appID.String() + "/download", permission: accesscontrol.PermissionAppRead},
		{method: http.MethodGet, path: "/apps/" + appID.String() + "/openapi", permission: accesscontrol.PermissionAppRead},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, nil)
		request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
		requirements, _, ok := resolveControlRESTPolicy(request, resolver)
		if !ok || len(requirements) != 1 {
			t.Fatalf("%s %s requirements = %#v, ok=%v", test.method, test.path, requirements, ok)
		}
		want := accesscontrol.Requirement{Permission: test.permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}}
		if requirements[0] != want {
			t.Fatalf("%s %s requirement = %#v, want %#v", test.method, test.path, requirements[0], want)
		}
	}
}

func TestAppTokenAccessRequirementsUseFamilyBoundary(t *testing.T) {
	accountID, familyID := uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{families: map[uuid.UUID]store.AppFamily{
		familyID: {AppFamilyID: familyID, AccountID: accountID},
	}}
	resolver := newControlRequirementResolver(stores, nil)
	request := httptest.NewRequest(http.MethodPost, "/workspace/app-tokens?app_family_id="+familyID.String(), nil)
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), accesscontrol.Actor{AccountID: accountID}))
	requirements, _, ok := resolveControlRESTPolicy(request, resolver)
	want := accesscontrol.Requirement{Permission: accesscontrol.PermissionAppTokensManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}}
	if !ok || len(requirements) != 1 || requirements[0] != want {
		t.Fatalf("requirements = %#v, ok=%v, want %#v", requirements, ok, want)
	}
}

func (s *controlRequirementStoreStub) GetAppFamily(_ context.Context, familyID uuid.UUID) (*store.AppFamily, error) {
	if family, ok := s.families[familyID]; ok {
		copy := family
		return &copy, nil
	}
	return nil, store.ErrAppFamilyNotFound
}

func (s *controlRequirementStoreStub) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	s.appLoads++
	if app, ok := s.apps[appID]; ok {
		copy := app
		return &copy, nil
	}
	return nil, store.ErrAppNotFound
}

// ResolveWorkspaceServiceIDsByKeys stands in for the Engine's local
// fused_workspace_services mirror: only keys present in localServiceIDs
// resolve, exactly like a real cache miss for a service never added to this
// workspace before.
func (s *controlRequirementStoreStub) ResolveWorkspaceServiceIDsByKeys(_ context.Context, keys []string) (map[string]uuid.UUID, error) {
	s.slugLoads++
	resolved := make(map[string]uuid.UUID, len(keys))
	for _, key := range keys {
		if id, ok := s.localServiceIDs[key]; ok {
			resolved[key] = id
		}
	}
	return resolved, nil
}

func (s *controlRequirementStoreStub) GetBucketByName(context.Context, string) (*store.Bucket, error) {
	s.bucketLoads++
	return s.bucket, nil
}

func (s *controlRequirementStoreStub) LoadDefaultBucketID(context.Context) (uuid.UUID, error) {
	s.defaultLoads++
	return s.defaultID, nil
}

func (s *controlRequirementStoreStub) ListWorkspaceServices(context.Context, []string) ([]store.WorkspaceService, error) {
	s.serviceLoads++
	return s.services, nil
}

func (s *controlRequirementStoreStub) GetBucketsByNames(context.Context, []string) ([]store.Bucket, error) {
	s.batchBuckets++
	return s.buckets, nil
}

func (s *controlRequirementStoreStub) ResolveAuthorizationResourceDisplayNames(_ context.Context, resources []accesscontrol.ResourceRef) (map[accesscontrol.ResourceRef]string, error) {
	s.displayLoads++
	resolved := make(map[accesscontrol.ResourceRef]string, len(resources))
	for _, resource := range resources {
		if name := s.displayNames[resource]; name != "" {
			resolved[resource] = name
		}
	}
	return resolved, nil
}

func TestEnrichControlPermissionDenialResolvesOnlyReadableResources(t *testing.T) {
	workspaceID, readableBucket, hiddenBucket := uuid.New(), uuid.New(), uuid.New()
	readableRef := accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: readableBucket}
	hiddenRef := accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: hiddenBucket}
	actor := actorWithGrants(t, workspaceID, accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: readableRef})
	authorizer := accesscontrol.SnapshotAuthorizer{}
	denied := &accesscontrol.PermissionDeniedError{Missing: []accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionBucketUse, Resource: readableRef},
		{Permission: accesscontrol.PermissionBucketUse, Resource: hiddenRef},
	}}
	stores := &controlRequirementStoreStub{displayNames: map[accesscontrol.ResourceRef]string{
		readableRef: "payments-production", hiddenRef: "must-not-leak",
	}}
	resolver := newControlRequirementResolver(stores, nil)
	got := enrichControlPermissionDenial(context.Background(), actor, authorizer, resolver, denied)
	var enriched *accesscontrol.PermissionDeniedError
	if !errors.As(got, &enriched) {
		t.Fatalf("error = %v", got)
	}
	if enriched.DisplayNames[readableRef] != "payments-production" || enriched.DisplayNames[hiddenRef] != "" || stores.displayLoads != 1 {
		t.Fatalf("display names/loads = %#v/%d", enriched.DisplayNames, stores.displayLoads)
	}
}

type controlConfigRepositoryStub struct {
	store.ConfigRepository
	plan       *store.ConfigPlan
	state      *store.ConfigState
	planLoads  int
	stateLoads int
	stateKey   string
}

func (s *controlConfigRepositoryStub) GetConfigPlan(context.Context, uuid.UUID) (*store.ConfigPlan, error) {
	s.planLoads++
	return s.plan, nil
}

func (s *controlConfigRepositoryStub) GetConfigState(_ context.Context, key string) (*store.ConfigState, error) {
	s.stateLoads++
	s.stateKey = key
	return s.state, nil
}

func TestDynamicServiceCreateRequirementRestoresBody(t *testing.T) {
	serviceID := uuid.New()
	body := `{"service_id":"` + serviceID.String() + `","service_name":"Example"}`
	request := httptest.NewRequest(http.MethodPost, "/workspace/services", strings.NewReader(body))
	resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, &controlConfigRepositoryStub{})

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{}, dynamicServiceCreate, nil, request)
	if err != nil || len(requirements) != 1 || requirements[0].Resource.ID != serviceID {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	restored, _ := io.ReadAll(request.Body)
	if string(restored) != body {
		t.Fatalf("restored body = %q, want %q", restored, body)
	}
}

func TestDynamicSecretRequirementUsesPointDefaultBucketLookup(t *testing.T) {
	bucketID := uuid.New()
	stores := &controlRequirementStoreStub{defaultID: bucketID}
	resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{})
	request := httptest.NewRequest(http.MethodPut, "/workspace/secrets", strings.NewReader(`{"service_id":"`+uuid.NewString()+`"}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{}, dynamicSecretWrite, nil, request)
	if err != nil || len(requirements) != 1 || requirements[0].Resource.ID != bucketID {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if stores.defaultLoads != 1 || stores.bucketLoads != 0 {
		t.Fatalf("default/bucket loads = %d/%d, want 1/0", stores.defaultLoads, stores.bucketLoads)
	}
}

func TestDynamicWorkspaceApplyBatchesPlanActionResolution(t *testing.T) {
	workspaceID := uuid.New()
	firstServiceID := uuid.New()
	secondServiceID := uuid.New()
	planID := uuid.New()
	configStore := &controlConfigRepositoryStub{plan: &store.ConfigPlan{
		ID: planID, ConfigType: store.ConfigTypeWorkspace, Revision: 7,
		Actions: []byte(`[{"type":"add_service","service_id":"` + firstServiceID.String() + `"},{"type":"remove_service","service_id":"` + secondServiceID.String() + `"},{"type":"create_bucket_binding"}]`),
	}}
	resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, configStore)
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", strings.NewReader(`{"plan_id":"`+planID.String()+`"}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspaceApply, nil, request)
	if err != nil || len(requirements) != 4 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if configStore.planLoads != 1 || configStore.stateLoads != 0 {
		t.Fatalf("plan/state loads = %d/%d, want 1/0", configStore.planLoads, configStore.stateLoads)
	}
	if revision, ok := api.AuthorizedPlanRevisionFromContext(request.Context()); !ok || revision != 7 {
		t.Fatalf("authorized plan revision = %d, %v; want 7, true", revision, ok)
	}
}

func TestDynamicDesiredConfigApplyBindsAuthorizedPlanRevision(t *testing.T) {
	for _, test := range []struct {
		configType store.ConfigType
		path       string
	}{
		{configType: store.ConfigTypeSDK, path: "/sdk-config/apply"},
		{configType: store.ConfigTypeMCP, path: "/mcp-config/apply"},
		{configType: store.ConfigTypeWebhook, path: "/webhook-config/apply"},
	} {
		t.Run(string(test.configType), func(t *testing.T) {
			workspaceID, planID, serviceID, bucketID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			required, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{{
				Permission: accesscontrol.PermissionServiceConsume,
				Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID},
			}})
			configStore := &controlConfigRepositoryStub{plan: &store.ConfigPlan{
				ID: planID, ConfigType: test.configType, Revision: 9,
				ResolvedPayload:     []byte(`{"bucket_id":"` + bucketID.String() + `","selections":[{"service_id":"` + serviceID.String() + `"}]}`),
				DesiredState:        []byte(`{"services":{"github":{}}}`),
				RequiredPermissions: required,
			}}
			stores := &controlRequirementStoreStub{services: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github"}}}
			resolver := newControlRequirementResolver(stores, configStore)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"plan_id":"`+planID.String()+`"}`))

			_, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicDesiredConfigApply, nil, request)
			if err != nil {
				t.Fatalf("ResolveControlRequirements: %v", err)
			}
			if revision, ok := api.AuthorizedPlanRevisionFromContext(request.Context()); !ok || revision != 9 {
				t.Fatalf("authorized plan revision = %d, %v; want 9, true", revision, ok)
			}
		})
	}
}

func TestDynamicWorkspaceApplyScopesCredentialMaterialsToBucket(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	planID := uuid.New()
	stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: bucketID, Name: "production"}}}
	configStore := &controlConfigRepositoryStub{plan: &store.ConfigPlan{ID: planID, ConfigType: store.ConfigTypeWorkspace, Actions: []byte("[]")}}
	resolver := newControlRequirementResolver(stores, configStore)
	body := `{"plan_id":"` + planID.String() + `","auth_materials":{"production\u0000payments":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/apply", strings.NewReader(body))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspaceApply, nil, request)
	if err != nil || len(requirements) != 2 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if requirements[1].Permission != accesscontrol.PermissionCredentialsManage || requirements[1].Resource.ID != bucketID {
		t.Fatalf("credential requirement = %#v", requirements[1])
	}
	if stores.batchBuckets != 1 {
		t.Fatalf("bucket batch loads = %d, want 1", stores.batchBuckets)
	}
}

func TestDynamicDesiredConfigApplyChoosesCreateOrManage(t *testing.T) {
	workspaceID := uuid.New()
	appID := uuid.New()
	serviceID := uuid.New()
	bucketID := uuid.New()
	planID := uuid.New()
	tests := []struct {
		name       string
		generation int
		state      *store.ConfigState
		permission accesscontrol.Permission
		resource   accesscontrol.ResourceType
		stateLoads int
	}{
		{name: "create", permission: accesscontrol.PermissionAppCreate, resource: accesscontrol.ResourceWorkspace},
		{name: "manage", generation: 2, state: &store.ConfigState{LatestResourceID: &appID}, permission: accesscontrol.PermissionAppManage, resource: accesscontrol.ResourceApp, stateLoads: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configStore := &controlConfigRepositoryStub{
				plan: &store.ConfigPlan{
					ID: planID, ConfigKey: "sdk:test", ConfigType: store.ConfigTypeSDK, BaseGeneration: test.generation,
					ResolvedPayload: []byte(`{"bucket_id":"` + bucketID.String() + `","selections":[{"service_id":"` + serviceID.String() + `"}]}`), DesiredState: []byte(`{"bucket":"production"}`),
				},
				state: test.state,
			}
			stores := &controlRequirementStoreStub{}
			if test.state != nil {
				stores.apps = map[uuid.UUID]store.App{
					appID: {AppID: appID, AppFamilyID: appID},
				}
			}
			resolver := newControlRequirementResolver(stores, configStore)
			request := httptest.NewRequest(http.MethodPost, "/sdk-config/apply", strings.NewReader(`{"plan_id":"`+planID.String()+`"}`))
			requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicDesiredConfigApply, nil, request)
			if err != nil || len(requirements) != 3 {
				t.Fatalf("requirements/error = %#v/%v", requirements, err)
			}
			if requirements[0].Permission != test.permission || requirements[0].Resource.Type != test.resource {
				t.Fatalf("requirement = %#v", requirements[0])
			}
			if requirements[2].Permission != accesscontrol.PermissionBucketUse || requirements[2].Resource.ID != bucketID {
				t.Fatalf("stored bucket requirement = %#v", requirements[2])
			}
			if configStore.planLoads != 1 || configStore.stateLoads != test.stateLoads {
				t.Fatalf("plan/state loads = %d/%d, want 1/%d", configStore.planLoads, configStore.stateLoads, test.stateLoads)
			}
		})
	}
}

func TestDynamicDesiredConfigPlanResolvesSelectionsInBatches(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	bucketID := uuid.New()
	stores := &controlRequirementStoreStub{
		services: []store.WorkspaceService{{ServiceID: serviceID}},
		buckets:  []store.Bucket{{ID: bucketID, Name: "production"}},
	}
	resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{})
	body := `{"config_key":"sdk:payments:1.0.0","config":{"bucket":"production","services":{"payments":{"version":"1"}}}}`
	request := httptest.NewRequest(http.MethodPost, "/sdk-config/plan", strings.NewReader(body))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicDesiredConfigPlan, nil, request)
	if err != nil || len(requirements) != 3 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	configStore := resolver.(*storeBackedControlRequirementResolver).configStore.(*controlConfigRepositoryStub)
	if stores.serviceLoads != 1 || stores.batchBuckets != 1 || configStore.stateLoads != 1 || configStore.stateKey != "sdk:payments:1.0.0" {
		t.Fatalf("service/bucket/state loads = %d/%d/%d key=%q", stores.serviceLoads, stores.batchBuckets, configStore.stateLoads, configStore.stateKey)
	}
}

func TestWebhookPlanSecretReferencesRequireEveryBucketReadBeforeSideEffects(t *testing.T) {
	workspaceID, serviceID, auditServiceID := uuid.New(), uuid.New(), uuid.New()
	alphaID, betaID := uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{
		services: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github"}, {ServiceID: auditServiceID, ServiceName: "github-audit"}},
		buckets: []store.Bucket{
			{ID: alphaID, Name: "alpha"},
			{ID: betaID, Name: "beta"},
		},
	}
	resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{})
	body := `{"config_key":"webhook:security","config":{"services":{"github":{"secret":"${bucket.alpha.secret.one}"},"github-audit":{"secret":"${bucket.beta.secret.two}"}}}}`
	actor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: auditServiceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: alphaID}},
	)
	planWrites, registrationWrites, registryMutations := 0, 0, 0
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		planWrites++
		registrationWrites++
		registryMutations++
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || planWrites != 0 || registrationWrites != 0 || registryMutations != 0 {
		t.Fatalf("denied status/side effects = %d/%d/%d/%d", response.Code, planWrites, registrationWrites, registryMutations)
	}
	if stores.serviceLoads != 1 || stores.batchBuckets != 1 {
		t.Fatalf("service/bucket batches = %d/%d, want 1/1", stores.serviceLoads, stores.batchBuckets)
	}
	allowed := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: auditServiceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: alphaID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionBucketRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: betaID}},
	)
	request = httptest.NewRequest(http.MethodPost, "/webhook-config/plan", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), allowed))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || planWrites != 1 || registrationWrites != 1 || registryMutations != 1 {
		t.Fatalf("allowed status/side effects = %d/%d/%d/%d", response.Code, planWrites, registrationWrites, registryMutations)
	}
}

func TestWebhookPlanRejectsInvalidSecretBeforeSideEffectsWithoutEcho(t *testing.T) {
	workspaceID, serviceID := uuid.New(), uuid.New()
	stores := &controlRequirementStoreStub{services: []store.WorkspaceService{{ServiceID: serviceID, ServiceName: "github"}}}
	resolver := newControlRequirementResolver(stores, &controlConfigRepositoryStub{})
	credential := "test-provider-credential-material"
	body := `{"config_key":"webhook:security","config":{"services":{"github":{"secret":"` + credential + `"}}}}`
	downstream := 0
	handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { downstream++ }))
	request := httptest.NewRequest(http.MethodPost, "/webhook-config/plan", strings.NewReader(body))
	request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
	)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || downstream != 0 || strings.Contains(response.Body.String(), credential) {
		t.Fatalf("invalid ref status/downstream/body = %d/%d/%q", response.Code, downstream, response.Body.String())
	}
	if stores.batchBuckets != 0 {
		t.Fatalf("invalid secret reached bucket query: %d", stores.batchBuckets)
	}
}

func TestWebhookApplyUsesPersistedImmutableBucketRequirement(t *testing.T) {
	workspaceID, planID, serviceID := uuid.New(), uuid.New(), uuid.New()
	originalBucketID := uuid.New()
	required, _ := accesscontrol.MarshalRequiredPermissions([]accesscontrol.Requirement{
		{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		{Permission: accesscontrol.PermissionServiceConsume, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}},
		{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: originalBucketID}},
	})
	configStore := &controlConfigRepositoryStub{plan: &store.ConfigPlan{
		ID: planID, ConfigType: store.ConfigTypeWebhook, Revision: 4,
		DesiredState:        []byte(`{"services":{"github":{"secret":"${bucket.production.secret.signing}"}}}`),
		RequiredPermissions: required,
	}}
	stores := &controlRequirementStoreStub{buckets: []store.Bucket{{ID: uuid.New(), Name: "production"}}}
	resolver := newControlRequirementResolver(stores, configStore)
	request := httptest.NewRequest(http.MethodPost, "/webhook-config/apply", strings.NewReader(`{"plan_id":"`+planID.String()+`"}`))
	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicDesiredConfigApply, nil, request)
	if err != nil {
		t.Fatalf("ResolveControlRequirements: %v", err)
	}
	found := false
	for _, requirement := range requirements {
		if requirement.Permission == accesscontrol.PermissionBucketUse && requirement.Resource.ID == originalBucketID {
			found = true
		}
	}
	if !found || stores.batchBuckets != 0 {
		t.Fatalf("requirements/bucket lookups = %#v/%d", requirements, stores.batchBuckets)
	}
}

func TestDynamicSDKGenerateRequiresEverySelectionAndBucket(t *testing.T) {
	firstServiceID := uuid.New()
	secondServiceID := uuid.New()
	bucketID := uuid.New()
	resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, &controlConfigRepositoryStub{})
	body := `{"selections":[{"service_id":"` + firstServiceID.String() + `"},{"service_id":"` + secondServiceID.String() + `"}],"bucket_id":"` + bucketID.String() + `"}`
	request := httptest.NewRequest(http.MethodPost, "/sdks/generate", strings.NewReader(body))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{}, dynamicSDKGenerate, nil, request)
	if err != nil || len(requirements) != 3 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if requirements[0].Permission != accesscontrol.PermissionServiceConsume || requirements[2].Permission != accesscontrol.PermissionBucketUse {
		t.Fatalf("requirements = %#v", requirements)
	}
}

func TestWorkspacePlanIgnoresUnchangedResources(t *testing.T) {
	workspaceID := uuid.New()
	unchangedID := uuid.New()
	changedID := uuid.New()
	current := []byte(`{"services":{"stable":{"service_id":"` + unchangedID.String() + `","version":"1"},"changed":{"service_id":"` + changedID.String() + `","version":"1"}},"buckets":{}}`)
	desired := `{"services":{"stable":{"service_id":"` + unchangedID.String() + `","version":"1"},"changed":{"service_id":"` + changedID.String() + `","version":"2"}},"buckets":{}}`
	configStore := &controlConfigRepositoryStub{state: &store.ConfigState{DesiredState: current}}
	resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, configStore)
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", strings.NewReader(`{"config_key":"workspace","config":`+desired+`}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspacePlan, nil, request)
	if err != nil || len(requirements) != 2 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if requirements[1].Resource.ID != changedID {
		t.Fatalf("changed service requirement = %#v, want %s", requirements[1], changedID)
	}
}

// TestWorkspacePlanResolvesServiceSlugLocally covers the ordinary case: a
// workspace.yaml declares a service by bare slug (no service_id), and that
// slug is already known locally because it was added to this workspace by an
// earlier apply. Before the fix, addChangedServiceIDs would deny this
// request outright because the raw entry had no service_id yet -- the
// resolver never got a chance to run resolveWorkspaceServiceSlugs, because
// that only happens later, inside the handler this middleware guards.
func TestWorkspacePlanResolvesServiceSlugLocally(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	configStore := &controlConfigRepositoryStub{}
	stores := &controlRequirementStoreStub{localServiceIDs: map[string]uuid.UUID{"stripe": serviceID}}
	resolver := newControlRequirementResolver(stores, configStore)
	desired := `{"services":{"stripe":{"version":"1"}},"buckets":{}}`
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", strings.NewReader(`{"config_key":"workspace","config":`+desired+`}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspacePlan, nil, request)
	if err != nil {
		t.Fatalf("ResolveControlRequirements: %v", err)
	}
	found := false
	for _, requirement := range requirements {
		if requirement.Permission == accesscontrol.PermissionServiceManage && requirement.Resource.ID == serviceID {
			found = true
		}
	}
	if !found || stores.slugLoads != 1 {
		t.Fatalf("requirements/slugLoads = %#v/%d", requirements, stores.slugLoads)
	}
	// This resolver only mutates its own decoded copy of the document -- the
	// original request body (which the handler re-reads to do its own,
	// independent resolution) must come back byte-for-byte unchanged.
	restored, _ := io.ReadAll(request.Body)
	if strings.Contains(string(restored), "service_id") {
		t.Fatalf("original request body was mutated: %s", restored)
	}
}

// TestWorkspacePlanRequiresWorkspaceAuthorityForNewService covers adding a
// service to the workspace for the first time: the slug has no local
// fused_workspace_services row (no Registry lookup happens -- this
// middleware never leaves the Engine's own store), so it can't be turned
// into a resource-scoped requirement. Instead the plan must require
// workspace-level service.manage, the same fallback bucketNameRequirements
// already uses for a bucket name it can't find. This is what actually
// unblocks "add a new service by slug" for an owner/admin -- not by
// resolving the slug's real ID up front, but by authorizing on workspace
// authority since no per-resource grant could exist yet anyway.
func TestWorkspacePlanRequiresWorkspaceAuthorityForNewService(t *testing.T) {
	workspaceID := uuid.New()
	configStore := &controlConfigRepositoryStub{}
	stores := &controlRequirementStoreStub{} // no local match: brand-new service
	resolver := newControlRequirementResolver(stores, configStore)
	desired := `{"services":{"new-service":{"version":"1"}},"buckets":{}}`
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", strings.NewReader(`{"config_key":"workspace","config":`+desired+`}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspacePlan, nil, request)
	if err != nil {
		t.Fatalf("ResolveControlRequirements: %v", err)
	}
	found := false
	for _, requirement := range requirements {
		if requirement.Permission == accesscontrol.PermissionServiceManage &&
			requirement.Resource.Type == accesscontrol.ResourceWorkspace &&
			requirement.Resource.ID == workspaceID {
			found = true
		}
	}
	if !found || stores.slugLoads != 1 {
		t.Fatalf("requirements/slugLoads = %#v/%d, want a workspace-scoped service.manage requirement", requirements, stores.slugLoads)
	}
}

// TestWorkspacePlanNewServiceRequiresWorkspaceAdmin proves the fallback end
// to end through the real authorization middleware: an actor holding only a
// narrow, resource-scoped service.manage grant (for a different, existing
// service) cannot add a brand-new service by slug, while an actor holding a
// workspace-scoped grant can.
func TestWorkspacePlanNewServiceRequiresWorkspaceAdmin(t *testing.T) {
	workspaceID := uuid.New()
	unrelatedServiceID := uuid.New()
	configStore := &controlConfigRepositoryStub{}
	stores := &controlRequirementStoreStub{}
	resolver := newControlRequirementResolver(stores, configStore)
	body := `{"config_key":"workspace","config":{"services":{"new-service":{"version":"1"}},"buckets":{}}}`

	run := func(actor accesscontrol.Actor) int {
		handler := controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{}, resolver)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		request := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", strings.NewReader(body))
		request = request.WithContext(accesscontrol.ContextWithActor(request.Context(), actor))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response.Code
	}

	narrowActor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: unrelatedServiceID}},
	)
	if code := run(narrowActor); code != http.StatusForbidden {
		t.Fatalf("narrowly-scoped actor status = %d, want 403", code)
	}

	adminActor := actorWithGrants(t, workspaceID,
		accesscontrol.Grant{Permission: accesscontrol.PermissionWorkspaceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}},
	)
	if code := run(adminActor); code != http.StatusOK {
		t.Fatalf("workspace-scoped actor status = %d, want 200", code)
	}
}

func TestWorkspacePlanRequiresServiceManageForDeprecation(t *testing.T) {
	workspaceID := uuid.New()
	serviceID := uuid.New()
	configStore := &controlConfigRepositoryStub{}
	resolver := newControlRequirementResolver(&controlRequirementStoreStub{}, configStore)
	desired := `{"services":{},"buckets":{},"deprecations":[{"service_id":"` + serviceID.String() + `","version":"1"}]}`
	request := httptest.NewRequest(http.MethodPost, "/workspace/config/plan", strings.NewReader(`{"config_key":"workspace","config":`+desired+`}`))

	requirements, err := resolver.ResolveControlRequirements(context.Background(), accesscontrol.Actor{WorkspaceID: workspaceID}, dynamicWorkspacePlan, nil, request)
	if err != nil || len(requirements) != 2 {
		t.Fatalf("requirements/error = %#v/%v", requirements, err)
	}
	if requirements[1].Permission != accesscontrol.PermissionServiceManage || requirements[1].Resource.ID != serviceID {
		t.Fatalf("deprecation requirement = %#v", requirements[1])
	}
}

func TestDynamicWorkspaceActionUnknownTypeFailsClosed(t *testing.T) {
	_, err := workspaceActionRequirements(uuid.New(), []byte(`[{"type":"future_unclassified_action"}]`))
	if err == nil {
		t.Fatal("unknown workspace action must fail closed")
	}
}
