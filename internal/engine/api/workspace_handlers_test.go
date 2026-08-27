package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// ── Mock store for workspace handler tests ────────────────────────────────────

// workspaceTestStore is a minimal store.Store stub that only implements the
// methods WorkspaceHandler exercises. The embedded store.Store provides zero
// implementations for every other method so the compiler is satisfied without
// repeating every stub here.
//
// Why embed instead of a full implementation: the workspace handlers only call
// GetAccountByAPIKey, GetWorkspaceIDForAccount, AddWorkspaceServiceVersion,
// ListWorkspaceServices, and RemoveWorkspaceService. Embedding keeps the test file
// focused on behavior under test, not boilerplate.
type workspaceTestStore struct {
	store.Store // zero-value embedded — panics if an uncovered method is called
	accountID   uuid.UUID
	workspaceID uuid.UUID
	appMutex    sync.Mutex
	appFamilies map[string]store.AppFamily
	apps        map[uuid.UUID]store.App
	tombstones  map[string]bool
	// tombstoneApps preserves only identity for historical activity tests; it
	// is intentionally separate from apps so catalogue/runtime reads stay empty.
	tombstoneApps map[uuid.UUID]store.App
	// workspaceServices returned by List
	workspaceServices []store.WorkspaceService
	// errors to inject
	activateErr   error
	deactivateErr error
	listErr       error
	workspaceErr  error
	// capture of the last AddWorkspaceServiceVersion call, so tests can assert what
	// was actually written (e.g. the Registry-verified name, not the client's).
	gotServiceName           string
	gotVersion               string
	gotServiceVersionID      uuid.UUID
	enabledVersion           string
	workspaceServiceVersions map[uuid.UUID][]store.WorkspaceServiceVersion
	versionLookupErr         error
	versionLookups           []uuid.UUID
	missingContractVersions  []store.WorkspaceServiceVersion
	missingContractLimit     int
	missingContractLookupErr error
	snapshotWrites           []store.ServiceContractSnapshot
	snapshotWriteErr         error
	removedVersions          []string
	removedWorkspaceServices []uuid.UUID
	// batchedVersionLookups records each ListWorkspaceServiceVersionsForServices
	// call's argument so tests can assert the batched path was used exactly
	// once instead of once per service.
	batchedVersionLookups      [][]uuid.UUID
	listWorkspaceServicesCalls int
	savedScopes                []sdkSaveParams
	saveScopeErr               error
	authorizationRevision      int64
	linkBucketCalls            int
	existingScopeHash          string
	existingScopeAccount       uuid.UUID
	existingScopeVersion       int
	existingScope              []byte
	mockScopes                 map[uuid.UUID]*store.AppRuntime
	listMCPScopesCalls         int
	// mcpAnalyticsDashboard/mcpAnalyticsErr drive GetMCPAnalyticsDashboard for
	// mcp_graphql_test.go; nil dashboard with nil err returns an empty one.
	mcpAnalyticsDashboard *models.MCPAnalyticsDashboard
	mcpAnalyticsErr       error
	batchedScopeLookups   [][]uuid.UUID
	scopeLookups          []uuid.UUID
	scopeBatchErr         error
	getScopeErr           error
	deletedScopes         []uuid.UUID
	// listWorkspaceWebhooksResult/Err let GraphQL tests drive webhook
	// visibility reads without a real DB.
	listWorkspaceWebhooksResult []store.WorkspaceWebhook
	listWorkspaceWebhooksErr    error
	webhookEvents               []models.WebhookEvent
	webhookAnalytics            models.WebhookAnalytics
	webhookAnalyticsErr         error
	engineExecutionEvents       []models.EngineExecutionEvent
	engineExecutionAnalytics    models.EngineExecutionAnalytics
	engineExecutionAnalyticsErr error
	appExecutionAnalytics       models.AppExecutionAnalytics
	workspaceExecutionAnalytics models.WorkspaceExecutionAnalytics
	workspaceExecutionCalls     int
	bucketsByName               map[string]*store.Bucket
	bucketsByID                 map[uuid.UUID]*store.Bucket
	// secretsByKey/secretLookupKeys/secretLookupErr back GetSecret -- used by
	// the connect client_id/client_secret ${bucket...} bucket-ref apply tests.
	secretsByKey                 map[string]*store.WorkspaceSecret
	secretLookupKeys             []string
	secretLookupErr              error
	getMCPScopeByNameFunc        func(ctx context.Context, accountID uuid.UUID, name, version string) (*store.AppRuntime, error)
	buckets                      []store.Bucket
	contractEndpointBatches      [][]store.ServiceContractEndpointSelection
	contractEndpointMatches      []store.ServiceContractEndpointMatch
	contractEndpointErr          error
	bucketSummaries              []store.BucketSummary
	sdkBuckets                   map[uuid.UUID][]store.Bucket
	appRuntimesForBucket         map[uuid.UUID][]store.AppRuntime
	bucketServiceSummaries       map[uuid.UUID][]store.BucketServiceSummary
	bucketValues                 map[uuid.UUID][]store.BucketValue
	secretMetas                  map[uuid.UUID][]store.WorkspaceSecretMeta
	upsertedSecrets              []store.WorkspaceSecret
	workspaceAuthBindings        []store.WorkspaceAuthBinding
	workspaceAuthPreflight       []store.WorkspaceAuthBinding
	workspaceAuthDesiredIDs      []uuid.UUID
	workspaceAuthPreflightErr    error
	bucketLookupNames            []string
	bucketBatchLookupNames       [][]string
	bucketBatchLookupErr         error
	bucketLookupErr              error
	upsertedConnectConfigs       []store.ConnectConfig
	upsertConnectConfigErr       error
	connectConfigs               map[string]*store.ConnectConfig
	connectConfigsForBucketErr   error
	connectConfigsForBucketCalls int
	secretMetaCalls              int
	appBucketReadinessCalls      int
	serviceConnectConfigs        []store.ConnectConfig
	workspaceConnectConfigs      []store.WorkspaceConnectConfig
	workspaceConnectProfiles     []store.WorkspaceConnectionProfile
	bucketConnectSummaries       map[uuid.UUID]*store.BucketConnectSummary
	authConnections              []store.AuthConnection
	getAuthConnectionsByIDsCalls int
	connectionResources          map[uuid.UUID][]store.ConnectionResource
	defaultConnectionResourceID  uuid.UUID
	deletedAuthConnections       []uuid.UUID
	deletedSecretKeys            []string
	createdConnectSessions       []store.ConnectSession
	appTokens                    []store.AppTokenMetadata
	// kind: webhook's ownership-conflict lookup controls.
	webhookOwnersByLabel   map[uuid.UUID]string
	webhookOwnersErr       error
	exactExecutionPolicies map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride
	packageBuildRequest    *models.SDKGenerationRequest
}

// ListServiceContractEndpointsForSelections models the one-query Engine snapshot resolver used by app plan tests.
func (s *workspaceTestStore) ListServiceContractEndpointsForSelections(_ context.Context, selections []store.ServiceContractEndpointSelection, _ []string) ([]store.ServiceContractEndpointMatch, error) {
	s.contractEndpointBatches = append(s.contractEndpointBatches, append([]store.ServiceContractEndpointSelection(nil), selections...))
	// Explicit fixture rows let Unified compilation tests control exact endpoint
	// schemas and identities without teaching this shared mock provider policy.
	if s.contractEndpointMatches != nil || s.contractEndpointErr != nil {
		return append([]store.ServiceContractEndpointMatch(nil), s.contractEndpointMatches...), s.contractEndpointErr
	}
	matches := make([]store.ServiceContractEndpointMatch, 0)
	for _, selection := range selections {
		for _, name := range selection.EndpointNames {
			// Stable synthetic IDs preserve immutable-selection assertions while the
			// production PostgreSQL resolver remains covered by store integration tests.
			endpointID := uuid.NewSHA1(selection.ServiceVersionID, []byte(name))
			matches = append(matches, store.ServiceContractEndpointMatch{
				SelectionIndex: selection.SelectionIndex,
				Endpoint:       fusedobject.Endpoint{ID: endpointID, Name: name},
			})
		}
	}
	return matches, nil
}

func (s *workspaceTestStore) CreateOrGetAppFamily(_ context.Context, family store.AppFamily) (*store.AppFamily, bool, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	if s.appFamilies == nil {
		s.appFamilies = make(map[string]store.AppFamily)
	}
	key := family.AccountID.String() + "\x00" + family.Kind.String() + "\x00" + family.CanonicalName
	if existing, ok := s.appFamilies[key]; ok {
		return &existing, false, nil
	}
	s.appFamilies[key] = family
	return &family, true, nil
}

// GetAppFamilyByIdentity looks up a family by the composite identity key used
// by CreateOrGetAppFamily.  This is needed by the SDK-generation path that
// checks family existence before enforcing the MaxSDKFamilies limit.
func (s *workspaceTestStore) GetAppFamilyByIdentity(_ context.Context, accountID uuid.UUID, kind, canonicalName string) (*store.AppFamily, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	key := accountID.String() + "\x00" + kind + "\x00" + canonicalName
	if existing, ok := s.appFamilies[key]; ok {
		copy := existing
		return &copy, nil
	}
	return nil, store.ErrAppFamilyNotFound
}

// CountAppFamilies returns the number of families for the given account and
// kind.  It is used by the license-tier limit check in reserveSDKGenerationIdentity.
func (s *workspaceTestStore) CountAppFamilies(_ context.Context, accountID uuid.UUID, kind string) (int, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	count := 0
	prefix := accountID.String() + "\x00" + kind + "\x00"
	for key := range s.appFamilies {
		if strings.HasPrefix(key, prefix) {
			count++
		}
	}
	return count, nil
}

// CountActiveServices returns the number of workspace services registered in
// the mock store.  It supports the license-tier service-limit check.
func (s *workspaceTestStore) CountActiveServices(_ context.Context) (int, error) {
	return len(s.workspaceServices), nil
}

func (s *workspaceTestStore) CountProjectedActiveServices(_ context.Context, desiredIDs, removableIDs []uuid.UUID) (int, int, error) {
	active := make(map[uuid.UUID]bool, len(s.workspaceServices))
	for _, service := range s.workspaceServices {
		active[service.ServiceID] = true
	}
	current := len(active)
	for _, serviceID := range removableIDs {
		delete(active, serviceID)
	}
	for _, serviceID := range desiredIDs {
		active[serviceID] = true
	}
	return current, len(active), nil
}

func (s *workspaceTestStore) GetAppByFamilyAndVersion(_ context.Context, familyID uuid.UUID, version string) (*store.App, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	for _, app := range s.apps {
		if app.AppFamilyID == familyID && app.Version == version {
			copy := app
			return &copy, nil
		}
	}
	return nil, store.ErrAppNotFound
}

func (s *workspaceTestStore) AppTombstoneExists(_ context.Context, familyID uuid.UUID, version string) (bool, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	return s.tombstones[familyID.String()+"\x00"+version], nil
}

func (s *workspaceTestStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	s.appMutex.Lock()
	app, exists := s.apps[appID]
	s.appMutex.Unlock()
	if exists {
		copy := app
		return &copy, nil
	}
	scope, ok := s.mockScopes[appID]
	if !ok {
		return nil, store.ErrAppNotFound
	}
	status := scope.Status
	if status == "" {
		status = "active"
	}
	return &store.App{
		AppID: appID, AppFamilyID: appID, AccountID: scope.AccountID,
		Version: scope.Version, ConfigKey: scope.ConfigKey, Status: status,
	}, nil
}

func (s *workspaceTestStore) ResolveAppFamilyAccess(_ context.Context, accountID uuid.UUID, appIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	s.appMutex.Lock()
	defer s.appMutex.Unlock()
	resolved := make(map[uuid.UUID]uuid.UUID, len(appIDs))
	for _, appID := range appIDs {
		if app, ok := s.apps[appID]; ok && app.AccountID == accountID {
			resolved[appID] = app.AppFamilyID
			continue
		}
		if scope := s.mockScopes[appID]; scope != nil && scope.AccountID == accountID {
			resolved[appID] = appID
			continue
		}
		if tombstone, ok := s.tombstoneApps[appID]; ok && tombstone.AccountID == accountID {
			resolved[appID] = tombstone.AppFamilyID
		}
	}
	return resolved, nil
}

func (s *workspaceTestStore) GetAppFamily(_ context.Context, appFamilyID uuid.UUID) (*store.AppFamily, error) {
	for _, family := range s.appFamilies {
		if family.AppFamilyID == appFamilyID {
			copy := family
			return &copy, nil
		}
	}
	for _, app := range s.apps {
		if app.AppFamilyID == appFamilyID {
			return &store.AppFamily{AppFamilyID: appFamilyID, AccountID: app.AccountID}, nil
		}
	}
	if scope := s.mockScopes[appFamilyID]; scope != nil {
		return &store.AppFamily{AppFamilyID: appFamilyID, AccountID: scope.AccountID, Kind: scope.Kind, DisplayName: scope.Name}, nil
	}
	return nil, store.ErrAppFamilyNotFound
}

func (s *workspaceTestStore) GetSDKPackageBuildRequest(_ context.Context, _ uuid.UUID, appID uuid.UUID) (*models.SDKGenerationRequest, error) {
	if s.packageBuildRequest == nil || s.packageBuildRequest.AppID != appID {
		return nil, store.ErrAppNotFound
	}
	return s.packageBuildRequest, nil
}

func (s *workspaceTestStore) DeprecateApp(_ context.Context, appID uuid.UUID, _ string, _ *time.Time) error {
	scope, ok := s.mockScopes[appID]
	if !ok {
		return store.ErrAppNotFound
	}
	scope.Status = "deprecated"
	return nil
}

func (s *workspaceTestStore) UndeprecateApp(_ context.Context, appID uuid.UUID) error {
	scope, ok := s.mockScopes[appID]
	if !ok {
		return store.ErrAppNotFound
	}
	scope.Status = "active"
	return nil
}

func (s *workspaceTestStore) DeactivateAppVersion(_ context.Context, appID, _ uuid.UUID) error {
	if _, ok := s.mockScopes[appID]; !ok {
		return store.ErrAppNotFound
	}
	delete(s.mockScopes, appID)
	s.deletedScopes = append(s.deletedScopes, appID)
	return nil
}

func (s *workspaceTestStore) GetWorkspaceExecutionPolicyOverrides(_ context.Context, _ []store.WorkspaceExecutionPolicyRef) (map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, error) {
	return s.exactExecutionPolicies, nil
}

func (s *workspaceTestStore) GetTeamBySlug(_ context.Context, slug string) (store.Team, error) {
	if slug == "" {
		return store.Team{}, store.ErrTeamNotFound
	}
	return store.Team{ID: testAppOwnerTeamID, Name: "Platform", Slug: slug, Status: store.TeamStatusActive}, nil
}

type sdkSaveParams struct {
	accountID          uuid.UUID
	appID              uuid.UUID
	ownerTeamID        uuid.UUID
	selections         []byte
	scopeSchemaVersion int
	kind               store.AppKind
	name               string
}

type mockVerifier struct {
	name              string
	currentVersionTag string
	serviceVersionID  uuid.UUID
	omitCurrentTag    bool
	err               error
	gotService        uuid.UUID
	gotAPIKey         string
	verifyCalls       int
	resolveCalls      int
	resolvedSlugs     []string
	slugIDs           map[string]uuid.UUID
	contractRevisions map[string]sandbox.ServiceVersionRevision
	latestVersions    map[uuid.UUID]sandbox.ServiceVersionResolvedRef
	latestBatches     [][]uuid.UUID
	// serviceMetadata lets tests supply an IncomingWebhookConfig/
	// EventExtractionPath for the webhook-registration apply path
	// (upsertWorkspaceServiceWebhooks). Defaults to an empty-but-non-nil
	// metadata object so callers that don't care about webhooks still get a
	// usable value instead of a nil pointer.
	serviceMetadata       *fusedobject.ServiceMetadata
	fetchMetadataErr      error
	fetchMetadataCalls    int
	fetchMetadataVersions []string
	// visibilityErr/visibilityCalls drive/observe FetchServiceVisibility for
	// Engine GraphQL workspaceServices display-slug lookups.
	visibilityErr      error
	visibilityCalls    [][]uuid.UUID
	authConfigVersions []sandbox.ServiceVersionAuthConfigs
	authConfigCalls    [][]sandbox.ServiceVersionRef
	authConfigErr      error
	// visibilityOverrides lets a test control the full ServiceVisibility
	// (IsOwner/IsPublic/Provider/Slug) for a specific ID -- needed to
	// exercise displaySlug's owner/public-gated provider qualification. IDs
	// not present here fall back to the default-non-owner-empty-slug
	// behavior below, which other (non-slug) tests rely on for WillArchive
	// computations.
	visibilityOverrides map[uuid.UUID]sandbox.ServiceVisibility
	discoveryEndpoint   *fusedobject.Endpoint
}

type runtimeContractVerifier struct {
	*mockVerifier
	runtimeContract     *store.ServiceContractSnapshot
	runtimeContractErr  error
	runtimeContractArgs []runtimeContractFetchArgs
	batchRuntimeArgs    [][]store.WorkspaceServiceVersion
}

type runtimeContractFetchArgs struct {
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	version          string
	apiKey           string
}

// FetchEndpointByName is an optional connect-resource capability used only by
// tests whose service metadata declares automatic discovery.
func (m *mockVerifier) FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version, endpointName string) (*fusedobject.Endpoint, error) {
	if m.discoveryEndpoint == nil || m.discoveryEndpoint.Name != endpointName {
		return nil, errors.New("endpoint not found")
	}
	return m.discoveryEndpoint, nil
}

func (m *mockVerifier) FetchServiceVersionAuthConfigs(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionAuthConfigs, error) {
	m.authConfigCalls = append(m.authConfigCalls, append([]sandbox.ServiceVersionRef(nil), refs...))
	if m.authConfigErr != nil {
		return nil, m.authConfigErr
	}
	return m.authConfigVersions, nil
}

func (m *mockVerifier) FetchServiceVersionExecutionAuthContracts(_ context.Context, selections []sandbox.ServiceVersionExecutionAuthSelection, _ string) ([]sandbox.ServiceVersionExecutionAuthContract, error) {
	if m.authConfigErr != nil {
		return nil, m.authConfigErr
	}
	configs := make(map[uuid.UUID]fusedobject.AuthConfigs, len(m.authConfigVersions))
	for _, version := range m.authConfigVersions {
		configs[version.ServiceID] = version.AuthConfigs
	}
	return anonymousExecutionAuthContracts(selections, configs), nil
}

func anonymousExecutionAuthContracts(selections []sandbox.ServiceVersionExecutionAuthSelection, configs map[uuid.UUID]fusedobject.AuthConfigs) []sandbox.ServiceVersionExecutionAuthContract {
	contracts := make([]sandbox.ServiceVersionExecutionAuthContract, 0, len(selections))
	for _, selection := range selections {
		operations := make([]sandbox.OperationSecuritySummary, 0, len(selection.OperationNames))
		for _, name := range selection.OperationNames {
			operations = append(operations, anonymousOperation(name))
		}
		contracts = append(contracts, sandbox.ServiceVersionExecutionAuthContract{
			ServiceID: selection.ServiceID, Version: selection.Version,
			OperationNames: selection.OperationNames, SelectAll: selection.SelectAll,
			AuthConfigs: configs[selection.ServiceID], Operations: operations,
		})
	}
	return contracts
}

func (m *mockVerifier) VerifyServiceExists(ctx context.Context, serviceID uuid.UUID, apiKey string) (string, string, string, uuid.UUID, error) {
	m.verifyCalls++
	m.gotService = serviceID
	m.gotAPIKey = apiKey
	if m.err != nil {
		return "", "", "", uuid.Nil, m.err
	}
	name := m.name
	if name == "" {
		name = "Registry Service"
	}
	currentVersionTag := m.currentVersionTag
	if currentVersionTag == "" && !m.omitCurrentTag {
		currentVersionTag = "2026-07-09"
	}
	serviceVersionID := m.serviceVersionID
	if serviceVersionID == uuid.Nil {
		serviceVersionID = uuid.New()
	}
	return name, "test/test-service", currentVersionTag, serviceVersionID, nil
}

func (m *mockVerifier) FetchServiceVersionRevisions(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionRevision, error) {
	out := make([]sandbox.ServiceVersionRevision, 0, len(refs))
	for _, ref := range refs {
		if revision, ok := m.contractRevisions[ref.ServiceID.String()+"|"+ref.Version]; ok {
			out = append(out, revision)
			continue
		}
		serviceVersionID := m.serviceVersionID
		if serviceVersionID == uuid.Nil {
			serviceVersionID = uuid.New()
		}
		out = append(out, sandbox.ServiceVersionRevision{
			ServiceID: ref.ServiceID, Version: ref.Version, ServiceVersionID: serviceVersionID, Revision: 1,
		})
	}
	return out, nil
}

func (m *mockVerifier) FetchLatestServiceVersions(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]sandbox.ServiceVersionResolvedRef, error) {
	m.latestBatches = append(m.latestBatches, append([]uuid.UUID(nil), serviceIDs...))
	if m.err != nil {
		return nil, m.err
	}
	out := make([]sandbox.ServiceVersionResolvedRef, 0, len(serviceIDs))
	for _, serviceID := range serviceIDs {
		if ref, ok := m.latestVersions[serviceID]; ok {
			out = append(out, ref)
			continue
		}
		out = append(out, sandbox.ServiceVersionResolvedRef{
			ServiceID:        serviceID,
			Version:          "2026-07-09",
			ServiceVersionID: uuid.New(),
		})
	}
	return out, nil
}

func (m *mockVerifier) ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error) {
	m.resolveCalls++
	m.resolvedSlugs = append([]string(nil), slugs...)
	if m.err != nil {
		return nil, m.err
	}
	out := map[string]uuid.UUID{}
	for _, slug := range slugs {
		if id, ok := m.slugIDs[slug]; ok {
			out[slug] = id
		}
	}
	return out, nil
}

func (m *mockVerifier) FetchServiceMetadata(_ context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	m.fetchMetadataCalls++
	m.fetchMetadataVersions = append(m.fetchMetadataVersions, version)
	if m.fetchMetadataErr != nil {
		return nil, m.fetchMetadataErr
	}
	if m.serviceMetadata != nil {
		return m.serviceMetadata, nil
	}
	return &fusedobject.ServiceMetadata{ID: serviceID}, nil
}

func (m *mockVerifier) FetchServiceMetadataBatch(_ context.Context, refs []sandbox.ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	m.fetchMetadataCalls++
	if m.fetchMetadataErr != nil {
		return nil, m.fetchMetadataErr
	}
	result := make(map[string]*fusedobject.ServiceMetadata, len(refs))
	for _, ref := range refs {
		metadata := m.serviceMetadata
		if metadata == nil {
			metadata = &fusedobject.ServiceMetadata{ID: ref.ServiceID}
		}
		result[sandbox.ServiceMetadataRefKey(ref)] = metadata
	}
	return result, nil
}

func (m *runtimeContractVerifier) FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error) {
	m.runtimeContractArgs = append(m.runtimeContractArgs, runtimeContractFetchArgs{
		serviceID: serviceID, serviceVersionID: serviceVersionID, version: version, apiKey: apiKey,
	})
	if m.runtimeContractErr != nil {
		return nil, m.runtimeContractErr
	}
	if m.runtimeContract != nil {
		return m.runtimeContract, nil
	}
	return &store.ServiceContractSnapshot{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: version}, nil
}

func (m *runtimeContractVerifier) FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error) {
	m.batchRuntimeArgs = append(m.batchRuntimeArgs, append([]store.WorkspaceServiceVersion(nil), versions...))
	if m.runtimeContractErr != nil {
		return nil, m.runtimeContractErr
	}
	out := make([]store.ServiceContractSnapshot, 0, len(versions))
	for _, version := range versions {
		out = append(out, store.ServiceContractSnapshot{
			ServiceID:        version.ServiceID,
			ServiceVersionID: version.ServiceVersionID,
			Version:          version.Version,
			ContractHash:     "hash-" + version.ServiceVersionID.String(),
		})
	}
	return out, nil
}

// FetchServiceVisibility satisfies ServiceVisibilityResolver. Plan tests that
// involve removed services need visibility data (to compute WillArchive); the
// default returns non-owner, non-public, no slug for every ID so removal
// actions are labelled as workspace-only removals (WillArchive: false) and
// fetchServiceSlugsForListing degrades to an empty slug, unless overridden
// via visibilityOverrides.
func (m *mockVerifier) FetchServiceVisibility(_ context.Context, serviceIDs []uuid.UUID, _ string) (map[uuid.UUID]sandbox.ServiceVisibility, error) {
	m.visibilityCalls = append(m.visibilityCalls, append([]uuid.UUID(nil), serviceIDs...))
	if m.visibilityErr != nil {
		return nil, m.visibilityErr
	}
	out := make(map[uuid.UUID]sandbox.ServiceVisibility, len(serviceIDs))
	for _, id := range serviceIDs {
		if override, ok := m.visibilityOverrides[id]; ok {
			out[id] = override
			continue
		}
		out[id] = sandbox.ServiceVisibility{ServiceID: id, IsOwner: false}
	}
	return out, nil
}

func buildWorkspaceRouter(s *workspaceTestStore, verifier ServiceVerifier) http.Handler {
	r := newControlTestRouter(s.accountID)
	dummyMasterKey := []byte("12345678901234567890123456789012")
	r.Mount("/workspace", WorkspaceHandler(s, verifier, dummyMasterKey, s))
	return r
}

func jsonBody(v any) *bytes.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func (s *workspaceTestStore) GetAccountByAPIKey(ctx context.Context, apiKey string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *workspaceTestStore) VerifyWorkspaceOwner(ctx context.Context, accountID uuid.UUID) error {
	return s.workspaceErr
}

func (s *workspaceTestStore) AddWorkspaceServiceVersion(
	ctx context.Context,
	serviceID uuid.UUID,
	serviceSlug string,
	version string,
	serviceVersionID uuid.UUID,
	serviceName string,
	addedBy uuid.UUID,
) error {
	s.gotVersion = version
	s.gotServiceVersionID = serviceVersionID
	s.gotServiceName = serviceName
	s.enabledVersion = version
	return s.activateErr
}

func (s *workspaceTestStore) EnableWorkspaceServiceVersion(
	ctx context.Context,
	serviceID uuid.UUID,
	version string,
	serviceVersionID uuid.UUID,
	enabledBy uuid.UUID,
) error {
	s.enabledVersion = version
	return s.activateErr
}

func (s *workspaceTestStore) DisableWorkspaceServiceVersion(ctx context.Context, serviceID uuid.UUID, version string) error {
	s.removedVersions = append(s.removedVersions, serviceID.String()+":"+version)
	return nil
}

func (s *workspaceTestStore) ListWorkspaceServiceVersions(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceServiceVersion, error) {
	if s.workspaceServiceVersions == nil {
		return nil, nil
	}
	return s.workspaceServiceVersions[serviceID], nil
}

func (s *workspaceTestStore) GetWorkspaceServiceVersion(ctx context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceServiceVersion, error) {
	s.versionLookups = append(s.versionLookups, serviceVersionID)
	if s.versionLookupErr != nil {
		return nil, s.versionLookupErr
	}
	for _, version := range s.workspaceServiceVersions[serviceID] {
		if version.ServiceVersionID == serviceVersionID && version.Status != "deprecated" {
			return &version, nil
		}
	}
	return nil, store.ErrWorkspaceServiceVersionNotFound
}

func (s *workspaceTestStore) ListWorkspaceServiceVersionsMissingContractSnapshots(ctx context.Context, limit int) ([]store.WorkspaceServiceVersion, error) {
	s.missingContractLimit = limit
	if s.missingContractLookupErr != nil {
		return nil, s.missingContractLookupErr
	}
	return append([]store.WorkspaceServiceVersion(nil), s.missingContractVersions...), nil
}

func (s *workspaceTestStore) UpsertServiceContractSnapshot(ctx context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error) {
	s.snapshotWrites = append(s.snapshotWrites, snapshot)
	return &snapshot, s.snapshotWriteErr
}

// ListWorkspaceServiceVersionsForServices fakes the batched lookup by filtering
// the same workspaceServiceVersions fixture down to the requested IDs, so handler
// tests can assert on batching (one map, built from one call) without a
// real database.
func (s *workspaceTestStore) ListWorkspaceServiceVersionsForServices(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID][]store.WorkspaceServiceVersion, error) {
	s.batchedVersionLookups = append(s.batchedVersionLookups, serviceIDs)
	out := map[uuid.UUID][]store.WorkspaceServiceVersion{}
	for _, id := range serviceIDs {
		if versions, ok := s.workspaceServiceVersions[id]; ok {
			out[id] = versions
		}
	}
	return out, nil
}

// GetLatestWorkspaceServiceVersionByWorkspace returns the first fixture version
// because connect-session tests only need a deterministic workspace pin.
func (s *workspaceTestStore) GetLatestWorkspaceServiceVersionByWorkspace(ctx context.Context, serviceID uuid.UUID) (string, error) {
	versions := s.workspaceServiceVersions[serviceID]
	if len(versions) == 0 {
		return "", nil
	}
	return versions[0].Version, nil
}

func (s *workspaceTestStore) ListWorkspaceServices(ctx context.Context, names []string) ([]store.WorkspaceService, error) {
	s.listWorkspaceServicesCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.workspaceServices, nil
}

func (s *workspaceTestStore) IsWorkspaceServiceEnabled(ctx context.Context, serviceID uuid.UUID) (bool, error) {
	for _, service := range s.workspaceServices {
		if service.ServiceID == serviceID {
			return true, nil
		}
	}
	return len(s.workspaceServiceVersions[serviceID]) > 0, nil
}

func (s *workspaceTestStore) ListAuthorizedWorkspaceServices(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string) ([]store.WorkspaceService, error) {
	services, err := s.ListWorkspaceServices(ctx, names)
	if err != nil || scope.All {
		return services, err
	}
	return filterTestWorkspaceServices(services, scope.IDs), nil
}

func (s *workspaceTestStore) ResolveWorkspaceServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error) {
	resolved := make(map[string]uuid.UUID)
	for _, key := range keys {
		for _, service := range s.workspaceServices {
			if service.ServiceSlug == key || service.ServiceName == key {
				resolved[key] = service.ServiceID
				break
			}
		}
	}
	return resolved, nil
}

func filterTestWorkspaceServices(services []store.WorkspaceService, ids []uuid.UUID) []store.WorkspaceService {
	allowed := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		allowed[id] = struct{}{}
	}
	filtered := make([]store.WorkspaceService, 0, len(services))
	for _, service := range services {
		if _, ok := allowed[service.ServiceID]; ok {
			filtered = append(filtered, service)
		}
	}
	return filtered
}

// ListWorkspaceServicesPage mirrors postgresStore's ListWorkspaceServicesPage
// semantics (workspace_store.go) closely enough for GraphQL resolver tests:
// filter by service_name when names is non-empty, then page the (already
// filtered) result by limit/offset. total reflects the filtered count before
// paging, matching the real COUNT(*)-then-SELECT split.
func (s *workspaceTestStore) ListWorkspaceServicesPage(ctx context.Context, names []string, limit, offset int) ([]store.WorkspaceService, int, error) {
	s.listWorkspaceServicesCalls++
	if s.listErr != nil {
		return nil, 0, s.listErr
	}
	filtered := s.workspaceServices
	if len(names) > 0 {
		filtered = make([]store.WorkspaceService, 0, len(s.workspaceServices))
		for _, svc := range s.workspaceServices {
			if containsString(names, svc.ServiceName) {
				filtered = append(filtered, svc)
			}
		}
	}
	total := len(filtered)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if limit <= 0 || end > total {
		end = total
	}
	return filtered[offset:end], total, nil
}

func (s *workspaceTestStore) ListAuthorizedWorkspaceServicesPage(ctx context.Context, scope accesscontrol.AuthorizedScope, names []string, limit, offset int) ([]store.WorkspaceService, int, error) {
	if scope.All {
		return s.ListWorkspaceServicesPage(ctx, names, limit, offset)
	}
	filtered := filterTestWorkspaceServices(s.workspaceServices, scope.IDs)
	original := s.workspaceServices
	s.workspaceServices = filtered
	defer func() { s.workspaceServices = original }()
	return s.ListWorkspaceServicesPage(ctx, names, limit, offset)
}

func (s *workspaceTestStore) RemoveWorkspaceService(ctx context.Context, serviceID uuid.UUID) error {
	s.removedWorkspaceServices = append(s.removedWorkspaceServices, serviceID)
	return s.deactivateErr
}

// RemoveWorkspaceServices records composite reconciliation without imposing a
// test-only deletion order on auth-reference targets and sources.
func (s *workspaceTestStore) RemoveWorkspaceServices(_ context.Context, serviceIDs []uuid.UUID) error {
	s.removedWorkspaceServices = append(s.removedWorkspaceServices, serviceIDs...)
	return s.deactivateErr
}

func (s *workspaceTestStore) GetWorkspaceWebhookBySlug(ctx context.Context, slug string) (*store.WorkspaceWebhook, error) {
	return nil, store.ErrWorkspaceWebhookNotFound
}

func (s *workspaceTestStore) ListWorkspaceWebhooks(ctx context.Context, serviceID uuid.UUID) ([]store.WorkspaceWebhook, error) {
	return s.listWorkspaceWebhooksResult, s.listWorkspaceWebhooksErr
}

func (s *workspaceTestStore) ListWebhookEventsByService(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, limit, offset int, startDate, endDate *time.Time) ([]models.WebhookEvent, int64, error) {
	filtered := filterTestItems(s.webhookEvents, func(event models.WebhookEvent) bool {
		return webhookEventMatches(event, accountID, serviceID, eventName, startDate, endDate)
	})
	page, total := paginateTestItems(filtered, limit, offset)
	return page, total, nil
}

func webhookEventMatches(event models.WebhookEvent, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) bool {
	if event.AccountID != accountID || event.ServiceID != serviceID {
		return false
	}
	if !optionalTestStringMatches(event.EventType, eventName) {
		return false
	}
	return testTimeInRange(event.CreatedAt, startDate, endDate)
}

func (s *workspaceTestStore) GetWebhookAnalytics(ctx context.Context, accountID, serviceID uuid.UUID, eventName string, startDate, endDate *time.Time) (models.WebhookAnalytics, error) {
	if s.webhookAnalyticsErr != nil {
		return models.WebhookAnalytics{}, s.webhookAnalyticsErr
	}
	return s.webhookAnalytics, nil
}

func (s *workspaceTestStore) ListEngineExecutionEventsByService(ctx context.Context, filter store.EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filtered := filterTestItems(s.engineExecutionEvents, func(event models.EngineExecutionEvent) bool {
		return event.ServiceID == filter.ServiceID && engineExecutionEventMatches(event, filter)
	})
	page, total := paginateTestItems(filtered, filter.Limit, filter.Offset)
	return page, total, nil
}

func (s *workspaceTestStore) ListEngineExecutionEventsByApp(ctx context.Context, filter store.EngineExecutionFilter) ([]models.EngineExecutionEvent, int64, error) {
	filtered := filterTestItems(s.engineExecutionEvents, func(event models.EngineExecutionEvent) bool {
		return event.AppFamilyID == filter.AppFamilyID &&
			(filter.AppID == uuid.Nil || event.AppID == filter.AppID) &&
			engineExecutionEventMatches(event, filter)
	})
	page, total := paginateTestItems(filtered, filter.Limit, filter.Offset)
	return page, total, nil
}

// These helpers keep the mock's in-memory behavior aligned with the store
// contract while leaving each fake query small enough to audit independently.
func filterTestItems[T any](items []T, matches func(T) bool) []T {
	filtered := make([]T, 0, len(items))
	for _, item := range items {
		if matches(item) {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func paginateTestItems[T any](items []T, limit, offset int) ([]T, int64) {
	total := int64(len(items))
	if offset >= len(items) {
		return []T{}, total
	}
	return items[offset:min(offset+limit, len(items))], total
}

func engineExecutionEventMatches(event models.EngineExecutionEvent, filter store.EngineExecutionFilter) bool {
	return optionalTestStringMatches(event.Transport, filter.Transport) &&
		optionalTestStringMatches(event.Direction, filter.Direction) &&
		optionalTestStringMatches(event.Status, filter.Status) &&
		testTimeInRange(event.StartedAt, filter.StartDate, filter.EndDate)
}

func optionalTestStringMatches(actual, expected string) bool {
	return expected == "" || actual == expected
}

func testTimeInRange(value time.Time, startDate, endDate *time.Time) bool {
	if startDate != nil && value.Before(*startDate) {
		return false
	}
	return endDate == nil || !value.After(*endDate)
}

func (s *workspaceTestStore) GetEngineExecutionAnalyticsByService(ctx context.Context, filter store.EngineExecutionFilter) (models.EngineExecutionAnalytics, error) {
	if s.engineExecutionAnalyticsErr != nil {
		return models.EngineExecutionAnalytics{}, s.engineExecutionAnalyticsErr
	}
	return s.engineExecutionAnalytics, nil
}

func (s *workspaceTestStore) GetEngineExecutionAnalyticsByApp(ctx context.Context, filter store.EngineExecutionFilter) (models.AppExecutionAnalytics, error) {
	if s.engineExecutionAnalyticsErr != nil {
		return models.AppExecutionAnalytics{}, s.engineExecutionAnalyticsErr
	}
	return s.appExecutionAnalytics, nil
}

func (s *workspaceTestStore) GetWorkspaceExecutionAnalytics(ctx context.Context, accountID uuid.UUID, startDate, endDate time.Time) (models.WorkspaceExecutionAnalytics, error) {
	s.workspaceExecutionCalls++
	return s.workspaceExecutionAnalytics, nil
}

// TestAddService_MissingServiceID_400 verifies malformed activation input uses
// the correlated structured admission contract before any workspace write.
func TestAddService_MissingServiceID_400(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	router := buildWorkspaceRouter(s, &mockVerifier{})

	// Omit service_id from the body — handler must reject with 400.
	body := jsonBody(map[string]string{"service_name": "Stripe"})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	chimiddleware.RequestID(router).ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	var envelope workspaceConfigErrorResponse
	// Structured validation metadata must remain stable for CLI automation.
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode activation error: %v", err)
	}
	if envelope.Error.Code != "invalid_workspace_service_request" || envelope.Error.Message != "service_id is required" || envelope.Error.Phase != "request_admission" || envelope.Error.CommitState != "not_committed" || envelope.Error.RequestID == "" {
		t.Fatalf("activation error = %#v", envelope.Error)
	}
}

// TestAddService_UsesRegistryVerifiedName confirms the stored name comes from
// the Registry (via ServiceVerifier's return value), not whatever the client
// put in the request body -- the whole point of verifying before writing.
// The request body's service_name deliberately differs from the verifier's
// name so this test would fail if the handler ever went back to trusting the
// client-supplied value.
func TestAddService_UsesRegistryVerifiedName(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	verifier := &mockVerifier{name: "Registry-Authoritative-Name"}
	router := buildWorkspaceRouter(s, verifier)

	svcID := uuid.New()
	body := jsonBody(map[string]string{
		"service_id":   svcID.String(),
		"service_name": "whatever-the-client-made-up",
		"version_tag":  "2.1",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if verifier.gotService != svcID {
		t.Errorf("expected verifier to be called with service_id %s, got %s", svcID, verifier.gotService)
	}
	if verifier.gotAPIKey != "fsk_test" {
		t.Errorf("expected verifier to receive the caller's own API key, got %q", verifier.gotAPIKey)
	}
	if s.gotServiceName != "Registry-Authoritative-Name" {
		t.Errorf("expected the store to receive the Registry-verified name, got %q", s.gotServiceName)
	}
	// version is still client-supplied -- verification only vouches for
	// identity/existence, not which version the user intends to pin.
	if s.gotVersion != "2.1" {
		t.Errorf("expected version %q to pass through from the request, got %q", "2.1", s.gotVersion)
	}
}

func TestRefreshServiceContract_RefreshesExactActivatedVersion(t *testing.T) {
	exporter := setupTestTracer(t)
	accountID := uuid.New()
	workspaceID := uuid.New()
	serviceID := uuid.New()
	versionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   accountID,
		workspaceID: workspaceID,
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "2026-07-23", Status: "public"}},
		},
	}
	verifier := &runtimeContractVerifier{mockVerifier: &mockVerifier{}, runtimeContract: &store.ServiceContractSnapshot{
		ServiceID:        serviceID,
		ServiceVersionID: versionID,
		Version:          "2026-07-23",
		ContractHash:     "contract-hash",
	}}
	router := buildWorkspaceRouter(s, verifier)

	req := httptest.NewRequest(http.MethodPost, "/workspace/services/"+serviceID.String()+"/versions/"+versionID.String()+"/refresh", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(s.versionLookups) != 1 || s.versionLookups[0] != versionID {
		t.Fatalf("expected one exact version lookup, got %#v", s.versionLookups)
	}
	assertRuntimeContractFetch(t, verifier.runtimeContractArgs, runtimeContractFetchArgs{
		serviceID: serviceID, serviceVersionID: versionID, version: "2026-07-23", apiKey: "fsk_test",
	})
	if len(s.snapshotWrites) != 1 || s.snapshotWrites[0].ContractHash != "contract-hash" {
		t.Fatalf("expected one snapshot write, got %#v", s.snapshotWrites)
	}
	if !strings.Contains(rr.Body.String(), `"status":"refreshed"`) {
		t.Fatalf("expected refreshed response, got %s", rr.Body.String())
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "engine.workspace.refresh_runtime_contract" {
		t.Fatalf("expected one refresh span, got %#v", spans)
	}
}

func TestRefreshServiceContract_UnactivatedVersionDoesNotFetch(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	s := &workspaceTestStore{accountID: uuid.New(), workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{}}
	verifier := &runtimeContractVerifier{mockVerifier: &mockVerifier{}}
	router := buildWorkspaceRouter(s, verifier)

	req := httptest.NewRequest(http.MethodPost, "/workspace/services/"+serviceID.String()+"/versions/"+versionID.String()+"/refresh", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(verifier.runtimeContractArgs) != 0 || len(s.snapshotWrites) != 0 {
		t.Fatalf("unactivated version must not fetch or write, fetch=%#v writes=%#v", verifier.runtimeContractArgs, s.snapshotWrites)
	}
}

func TestRefreshServiceContract_FetchFailurePreservesSnapshot(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{
			serviceID: {{ServiceID: serviceID, ServiceVersionID: versionID, Version: "2026-07-23", Status: "public"}},
		},
	}
	verifier := &runtimeContractVerifier{mockVerifier: &mockVerifier{}, runtimeContractErr: context.DeadlineExceeded}
	router := buildWorkspaceRouter(s, verifier)

	req := httptest.NewRequest(http.MethodPost, "/workspace/services/"+serviceID.String()+"/versions/"+versionID.String()+"/refresh", nil)
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	if len(verifier.runtimeContractArgs) != 1 {
		t.Fatalf("expected one fetch attempt, got %#v", verifier.runtimeContractArgs)
	}
	if len(s.snapshotWrites) != 0 {
		t.Fatalf("fetch failure must preserve existing snapshot and skip writes, got %#v", s.snapshotWrites)
	}
}

func TestRefreshMissingServiceContracts_BackfillsMissingSnapshotsInBatch(t *testing.T) {
	exporter := setupTestTracer(t)
	firstServiceID := uuid.New()
	firstVersionID := uuid.New()
	secondServiceID := uuid.New()
	secondVersionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		missingContractVersions: []store.WorkspaceServiceVersion{
			{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, Version: "2026-07-22", Status: "public"},
			{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, Version: "2026-07-23", Status: "public"},
		},
	}
	verifier := &runtimeContractVerifier{mockVerifier: &mockVerifier{}}
	result, err := refreshMissingServiceContracts(context.Background(), s, verifier, refreshMissingContractsCall{
		accountID: s.accountID,
		apiKey:    "fsk_test",
		limit:     2,
	})
	if err != nil {
		t.Fatalf("refreshMissingServiceContracts() error = %v", err)
	}
	if s.missingContractLimit != 2 {
		t.Fatalf("expected bounded missing lookup limit 2, got %d", s.missingContractLimit)
	}
	assertBatchedRuntimeContractFetch(t, verifier, 2)
	if len(s.snapshotWrites) != 2 {
		t.Fatalf("expected two snapshot writes, got %#v", s.snapshotWrites)
	}
	if result.Missing != 2 || result.Refreshed != 2 {
		t.Fatalf("expected backfill response counts, got %#v", result)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Name != "engine.workspace.refresh_missing_runtime_contracts" {
		t.Fatalf("expected one missing-refresh span, got %#v", spans)
	}
}

func assertRuntimeContractFetch(t *testing.T, got []runtimeContractFetchArgs, want runtimeContractFetchArgs) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("expected one runtime contract fetch, got %#v", got)
	}
	if got[0] != want {
		t.Fatalf("unexpected runtime contract fetch args: got %#v, want %#v", got[0], want)
	}
}

func assertBatchedRuntimeContractFetch(t *testing.T, verifier *runtimeContractVerifier, wantVersions int) {
	t.Helper()
	if len(verifier.batchRuntimeArgs) != 1 || len(verifier.batchRuntimeArgs[0]) != wantVersions {
		t.Fatalf("expected one batched runtime-contract fetch of %d versions, got %#v", wantVersions, verifier.batchRuntimeArgs)
	}
	if len(verifier.runtimeContractArgs) != 0 {
		t.Fatalf("batch-capable verifier must not fall back to per-version fetches: %#v", verifier.runtimeContractArgs)
	}
}

func TestRefreshMissingServiceContracts_FetchFailureSkipsWrites(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
		missingContractVersions: []store.WorkspaceServiceVersion{
			{ServiceID: serviceID, ServiceVersionID: versionID, Version: "2026-07-23", Status: "public"},
		},
	}
	verifier := &runtimeContractVerifier{mockVerifier: &mockVerifier{}, runtimeContractErr: context.DeadlineExceeded}

	_, err := refreshMissingServiceContracts(context.Background(), s, verifier, refreshMissingContractsCall{
		accountID: s.accountID,
		apiKey:    "fsk_test",
		limit:     100,
	})
	var httpErr refreshHTTPError
	if !errors.As(err, &httpErr) || httpErr.status != http.StatusBadGateway {
		t.Fatalf("expected 502 refresh error, got %v", err)
	}
	if len(s.snapshotWrites) != 0 {
		t.Fatalf("fetch failure must skip snapshot writes, got %#v", s.snapshotWrites)
	}
}

func TestAddService_RejectsLegacyVersionField(t *testing.T) {
	s := &workspaceTestStore{accountID: uuid.New()}
	router := buildWorkspaceRouter(s, &mockVerifier{name: "Stripe", currentVersionTag: "2026-07-09"})
	body := jsonBody(map[string]string{
		"service_id": uuid.NewString(),
		"version":    "2.1",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestAddService_PinsToRegistryCurrentVersionWhenRequestOmitsVersion(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	verifier := &mockVerifier{name: "Stripe", currentVersionTag: "2026-07-09"}
	router := buildWorkspaceRouter(s, verifier)

	body := jsonBody(map[string]string{
		"service_id":   uuid.New().String(),
		"service_name": "Stripe",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.gotVersion != "2026-07-09" {
		t.Errorf("expected activation to pin Registry current version, got %q", s.gotVersion)
	}
}

func TestAddService_AcceptsVersionTagField(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	svcID := uuid.New()
	requestedServiceVersionID := uuid.New()
	router := buildWorkspaceRouter(s, &mockVerifier{
		name:              "Stripe",
		currentVersionTag: "2026-07-09",
		contractRevisions: map[string]sandbox.ServiceVersionRevision{
			svcID.String() + "|2026-07-08": {
				ServiceID: svcID, Version: "2026-07-08", ServiceVersionID: requestedServiceVersionID, Revision: 1,
			},
		},
	})

	body := jsonBody(map[string]string{
		"service_id":   svcID.String(),
		"service_name": "Stripe",
		"version_tag":  "2026-07-08",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.gotVersion != "2026-07-08" {
		t.Errorf("expected activation to pin requested version_tag, got %q", s.gotVersion)
	}
	if s.gotServiceVersionID != requestedServiceVersionID {
		t.Errorf("expected activation service_version_id %s, got %s", requestedServiceVersionID, s.gotServiceVersionID)
	}
}

func TestAddService_StoreEnablesPinnedVersion(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	router := buildWorkspaceRouter(s, &mockVerifier{name: "Stripe", currentVersionTag: "2026-07-09"})

	body := jsonBody(map[string]string{
		"service_id":  uuid.New().String(),
		"version_tag": "2026-07-08",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.enabledVersion != "2026-07-08" {
		t.Errorf("expected store activation to enable pinned version, got %q", s.enabledVersion)
	}
}

func TestAddService_RejectsWhenNoConcreteVersionAvailable(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	router := buildWorkspaceRouter(s, &mockVerifier{name: "Draft Only", omitCurrentTag: true})

	body := jsonBody(map[string]string{
		"service_id":   uuid.New().String(),
		"service_name": "Draft Only",
	})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if s.gotVersion != "" {
		t.Errorf("expected no activation write when version is unavailable, got %q", s.gotVersion)
	}
}

// TestAddService_ServiceNotFoundInRegistry_404 covers the "client sent a
// service_id the Registry doesn't know about (or won't show them)" case.
func TestAddService_ServiceNotFoundInRegistry_404(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	router := buildWorkspaceRouter(s, &mockVerifier{err: sandbox.ErrServiceNotFound})

	body := jsonBody(map[string]string{"service_id": uuid.New().String(), "service_name": "Ghost"})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestAddService_RegistryUnreachable_502 covers "the Registry is down" --
// distinct from ErrServiceNotFound, so the client can tell the difference
// between "that service doesn't exist" and "try again later".
func TestAddService_RegistryUnreachable_502(t *testing.T) {
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: uuid.New(),
	}
	router := buildWorkspaceRouter(s, &mockVerifier{err: errors.New("connection refused")})

	body := jsonBody(map[string]string{"service_id": uuid.New().String(), "service_name": "Stripe"})
	req := httptest.NewRequest(http.MethodPost, "/workspace/services", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502, got %d: %s", rr.Code, rr.Body.String())
	}
	var envelope workspaceConfigErrorResponse
	// Dependency classification must remain actionable without exposing transport prose.
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Registry error: %v", err)
	}
	if envelope.Error.Code != "registry_verification_failed" || envelope.Error.Category != "dependency" || !envelope.Error.Retryable || envelope.Error.CommitState != "not_committed" {
		t.Fatalf("Registry error = %#v", envelope.Error)
	}
	if strings.Contains(rr.Body.String(), "connection refused") {
		t.Fatalf("Registry error leaked transport cause: %s", rr.Body.String())
	}
}

// TestRemoveService_NotFound_404 verifies an authoritative missing membership
// is distinct from an ambiguous persistence failure.
func TestRemoveService_NotFound_404(t *testing.T) {
	s := &workspaceTestStore{
		accountID:     uuid.New(),
		workspaceID:   uuid.New(),
		deactivateErr: store.ErrWorkspaceServiceNotFound,
	}
	router := buildWorkspaceRouter(s, &mockVerifier{})

	req := httptest.NewRequest(http.MethodDelete, "/workspace/services/"+uuid.New().String(), nil)
	req.Header.Set("X-API-Key", "fsk_test")

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d: %s", rr.Code, rr.Body.String())
	}
	var envelope workspaceConfigErrorResponse
	// Repository not-found proves the delete did not commit.
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode removal error: %v", err)
	}
	if envelope.Error.Code != "workspace_service_not_found" || envelope.Error.Phase != "workspace_mutation" || envelope.Error.CommitState != "not_committed" {
		t.Fatalf("removal error = %#v", envelope.Error)
	}
}

// TestRemoveServiceReferencedCredentialConflict proves source dependency
// fencing is a reviewable conflict rather than an ambiguous persistence error.
func TestRemoveServiceReferencedCredentialConflict(t *testing.T) {
	testStore := &workspaceTestStore{
		accountID: uuid.New(), workspaceID: uuid.New(),
		deactivateErr: store.ErrWorkspaceAuthReferenceInUse,
	}
	router := buildWorkspaceRouter(testStore, &mockVerifier{})
	request := httptest.NewRequest(http.MethodDelete, "/workspace/services/"+uuid.NewString(), nil)
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	var envelope workspaceConfigErrorResponse
	// The response must explain the dependency and prove no deletion committed.
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode dependency conflict: %v", err)
	}
	if response.Code != http.StatusConflict || envelope.Error.Code != "workspace_auth_reference_in_use" || envelope.Error.CommitState != "not_committed" {
		t.Fatalf("dependency conflict = status %d, error %#v", response.Code, envelope.Error)
	}
}

// TestRemoveServiceStoreFailureHidesCauseAndReportsUnknownCommit verifies an
// unclassified delete failure does not claim rollback or expose store details.
func TestRemoveServiceStoreFailureHidesCauseAndReportsUnknownCommit(t *testing.T) {
	testStore := &workspaceTestStore{accountID: uuid.New(), workspaceID: uuid.New(), deactivateErr: errors.New("database secret=fsk_never_return")}
	router := buildWorkspaceRouter(testStore, &mockVerifier{})
	request := httptest.NewRequest(http.MethodDelete, "/workspace/services/"+uuid.NewString(), nil)
	request.Header.Set("X-API-Key", "fsk_test")
	response := httptest.NewRecorder()
	chimiddleware.RequestID(router).ServeHTTP(response, request)

	var envelope workspaceConfigErrorResponse
	// The shared mutation envelope must carry request and outcome correlation.
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode store failure: %v", err)
	}
	if response.Code != http.StatusInternalServerError || envelope.Error.Code != "workspace_service_remove_failed" || envelope.Error.CommitState != "unknown" || envelope.Error.RequestID == "" {
		t.Fatalf("store failure = status %d, error %#v", response.Code, envelope.Error)
	}
	// Raw persistence errors can contain secrets and stay behind the handler boundary.
	if strings.Contains(response.Body.String(), "fsk_never_return") || strings.Contains(response.Body.String(), "database secret") {
		t.Fatalf("store failure leaked cause: %s", response.Body.String())
	}
}

// ── S7-3 regression: workspaceID resolution ────────────────────────────────
//
// saveConfigHandler/getConfigHandler used to read workspaceID from
// r.Context().Value("workspaceID") -- a context key nothing in the Engine's
// router chain (cmd/engine/cmd/start.go) ever sets, unlike their sibling
// handlers in this same file (addServiceHandler/removeServiceHandler),
// which resolve it from the authenticated local Actor. That meant both
// handlers always returned 401
// Unauthorized in production, no matter how valid the caller's API key was --
// this code path had no test coverage, which is how it went unnoticed. These
// tests guard the fix: both handlers now resolve workspaceID the same way
// their siblings do.

func (s *workspaceTestStore) BatchCreateWebhookEvents(ctx context.Context, events []models.WebhookEvent) error {
	return nil
}

func (s *workspaceTestStore) BatchCreateEngineExecutionEvents(ctx context.Context, events []models.EngineExecutionEvent) error {
	return nil
}

func (s *workspaceTestStore) SaveAppRuntime(ctx context.Context, scope store.AppRuntime) error {
	s.savedScopes = append(s.savedScopes, sdkSaveParams{
		accountID:          scope.AccountID,
		appID:              scope.AppID,
		ownerTeamID:        scope.OwnerTeamID,
		selections:         append([]byte(nil), scope.Selections...),
		scopeSchemaVersion: scope.ScopeSchemaVersion,
		kind:               scope.Kind,
		name:               scope.Name,
	})
	if s.saveScopeErr == nil {
		_, alreadyExists := s.mockScopes[scope.AppID]
		// Mirror the real store's round-trip: a save-then-get must see what was
		// just saved, including the exact app-version status.
		if s.mockScopes == nil {
			s.mockScopes = make(map[uuid.UUID]*store.AppRuntime)
		}
		saved := scope
		if existing, ok := s.mockScopes[scope.AppID]; ok {
			saved.Status = existing.Status
		} else {
			saved.CreatedAt = time.Now()
			saved.Status = "active"
		}
		s.mockScopes[scope.AppID] = &saved
		if !alreadyExists {
			// A new scope also creates its owner binding in the real transaction.
			s.authorizationRevision++
		}
	}
	return s.saveScopeErr
}

func (s *workspaceTestStore) LoadAuthorizationRevision(context.Context) (int64, error) {
	return s.authorizationRevision, nil
}

func (s *workspaceTestStore) GetAppRuntime(ctx context.Context, appID uuid.UUID) (*store.AppRuntime, error) {
	s.scopeLookups = append(s.scopeLookups, appID)
	if s.getScopeErr != nil {
		return nil, s.getScopeErr
	}
	if s.mockScopes != nil {
		scope, ok := s.mockScopes[appID]
		if !ok {
			return nil, store.ErrAppRuntimeNotFound
		}
		return scope, nil
	}
	if s.existingScopeHash == "" {
		return nil, store.ErrAppRuntimeNotFound
	}
	accountID := s.accountID
	if s.existingScopeAccount != uuid.Nil {
		accountID = s.existingScopeAccount
	}
	version := s.existingScopeVersion
	if version == 0 {
		version = models.AppScopeSchemaVersion
	}
	return &store.AppRuntime{
		AccountID:          accountID,
		AppID:              appID,
		Selections:         append([]byte(nil), s.existingScope...),
		ScopeSchemaVersion: version,
	}, nil
}

// ListMCPAppsByAccount filters mockScopes by accountID+kind="mcp",
func (s *workspaceTestStore) GetMCPAppByName(ctx context.Context, accountID uuid.UUID, name, version string) (*store.AppRuntime, error) {
	if s.getMCPScopeByNameFunc != nil {
		return s.getMCPScopeByNameFunc(ctx, accountID, name, version)
	}
	return nil, store.ErrAppRuntimeNotFound
}

// newest-first by CreatedAt, mirroring postgresStore's real query closely
// enough for mcp_graphql_test.go's list resolver tests.
func (s *workspaceTestStore) ListMCPAppsByAccount(ctx context.Context, accountID uuid.UUID, limit, offset int) ([]store.AppRuntime, int, error) {
	s.listMCPScopesCalls++
	var matched []store.AppRuntime
	for _, scope := range s.mockScopes {
		if scope.AccountID == accountID && scope.Kind == "mcp" {
			matched = append(matched, *scope)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	total := len(matched)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

func (s *workspaceTestStore) ListAuthorizedMCPAppsByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]store.AppRuntime, int, error) {
	if scope.All {
		return s.ListMCPAppsByAccount(ctx, accountID, limit, offset)
	}
	allowed := make(map[uuid.UUID]struct{}, len(scope.IDs))
	for _, id := range scope.IDs {
		allowed[id] = struct{}{}
	}
	var matched []store.AppRuntime
	for _, artifact := range s.mockScopes {
		if _, ok := allowed[artifact.AppID]; ok && artifact.AccountID == accountID && artifact.Kind == "mcp" {
			matched = append(matched, *artifact)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	total := len(matched)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

func (s *workspaceTestStore) ListAuthorizedAppRuntimesByAccount(_ context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind string, limit, offset int) ([]store.AppRuntime, int, error) {
	allowed := make(map[uuid.UUID]struct{}, len(scope.IDs))
	for _, id := range scope.IDs {
		allowed[id] = struct{}{}
	}
	var matched []store.AppRuntime
	for _, artifact := range s.mockScopes {
		_, permitted := allowed[artifact.AppID]
		if artifact.AccountID == accountID && (kind == "" || artifact.Kind == store.AppKind(kind)) && (scope.All || permitted) {
			matched = append(matched, *artifact)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].CreatedAt.After(matched[j].CreatedAt) })
	total := len(matched)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return matched[offset:end], total, nil
}

func (s *workspaceTestStore) ListServiceConsumers(_ context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, serviceID uuid.UUID) ([]store.ServiceConsumer, error) {
	allowed := make(map[uuid.UUID]struct{}, len(scope.IDs))
	for _, id := range scope.IDs {
		allowed[id] = struct{}{}
	}
	consumers := make([]store.ServiceConsumer, 0)
	for _, artifact := range s.mockScopes {
		_, permitted := allowed[artifact.AppID]
		if artifact.AccountID != accountID || (!scope.All && !permitted) {
			continue
		}
		var selections models.SDKSelections
		if err := json.Unmarshal(artifact.Selections, &selections); err != nil {
			return nil, err
		}
		for _, selection := range selections {
			if selection.ServiceID == serviceID {
				status := artifact.Status
				if status == "" {
					status = "active"
				}
				consumers = append(consumers, store.ServiceConsumer{
					AppID: artifact.AppID, Name: artifact.Name, Version: artifact.Version, Kind: artifact.Kind,
					Status: status, ServiceVersionID: selection.ServiceVersionID, SelectAll: selection.SelectAll,
					OperationCount: len(selection.EndpointIDs) + len(selection.OperationNames),
					WebhookCount:   len(selection.WebhookIDs) + len(selection.WebhookNames), CreatedAt: artifact.CreatedAt,
				})
			}
		}
	}
	return consumers, nil
}

func (s *workspaceTestStore) GetMCPAnalyticsDashboard(ctx context.Context, appID uuid.UUID) (*models.MCPAnalyticsDashboard, error) {
	if s.mcpAnalyticsErr != nil {
		return nil, s.mcpAnalyticsErr
	}
	if s.mcpAnalyticsDashboard != nil {
		return s.mcpAnalyticsDashboard, nil
	}
	return &models.MCPAnalyticsDashboard{}, nil
}

func (s *workspaceTestStore) ListAppRuntimes(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]*store.AppRuntime, error) {
	s.batchedScopeLookups = append(s.batchedScopeLookups, append([]uuid.UUID(nil), appIDs...))
	if s.scopeBatchErr != nil {
		return nil, s.scopeBatchErr
	}
	out := make(map[uuid.UUID]*store.AppRuntime)
	for _, appID := range appIDs {
		scope, err := s.GetAppRuntime(ctx, appID)
		if err == nil {
			out[appID] = scope
			continue
		}
		if !errors.Is(err, store.ErrAppRuntimeNotFound) {
			return nil, err
		}
	}
	s.scopeLookups = nil
	return out, nil
}

func (s *workspaceTestStore) ListAppTokens(ctx context.Context, appFamilyID uuid.UUID) ([]store.AppTokenMetadata, error) {
	return s.appTokens, nil
}

func (s *workspaceTestStore) ListSecretMeta(ctx context.Context, bucketID uuid.UUID) ([]store.WorkspaceSecretMeta, error) {
	s.secretMetaCalls++
	if s.secretMetas != nil {
		return s.secretMetas[bucketID], nil
	}
	return nil, nil
}

func (s *workspaceTestStore) GetAppBucketCredentialPresence(_ context.Context, bucketID uuid.UUID, requirements []store.AppCredentialRequirement) ([]store.AppCredentialPresence, error) {
	s.appBucketReadinessCalls++
	if s.connectConfigsForBucketErr != nil {
		return nil, s.connectConfigsForBucketErr
	}
	presence := make([]store.AppCredentialPresence, 0, len(requirements))
	for _, requirement := range requirements {
		item := store.AppCredentialPresence{
			ServiceID: requirement.ServiceID, AuthType: requirement.AuthType, AuthName: requirement.AuthName,
		}
		if config := s.connectConfigs[bucketID.String()+":"+requirement.ServiceID.String()]; config != nil {
			item.Connected = config.Enabled && canonicalWorkspaceStaticAuthType(config.AuthType) == requirement.AuthType && strings.TrimSpace(config.AuthName) == requirement.AuthName
		}
		for _, requiredKey := range requirement.SecretKeys {
			for _, secret := range s.secretMetas[bucketID] {
				if secret.ServiceID == requirement.ServiceID && secret.KeyName == requiredKey {
					item.SecretKeys = append(item.SecretKeys, requiredKey)
				}
			}
		}
		presence = append(presence, item)
	}
	return presence, nil
}

func (s *workspaceTestStore) ListSecretMetaPage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]store.WorkspaceSecretMeta, int, error) {
	items, total := pageWorkspaceSecretMetas(s.secretMetas[bucketID], limit, offset)
	return items, total, nil
}

func (s *workspaceTestStore) UpsertSecret(ctx context.Context, secret store.WorkspaceSecret) error {
	s.upsertedSecrets = append(s.upsertedSecrets, secret)
	return nil
}

func (s *workspaceTestStore) UpsertSecrets(ctx context.Context, secrets []store.WorkspaceSecret) error {
	s.upsertedSecrets = append(s.upsertedSecrets, secrets...)
	return nil
}

// PreflightWorkspaceAuthBindings records read-only admission inputs so API
// tests can prove validation precedes every workspace mutation.
func (s *workspaceTestStore) PreflightWorkspaceAuthBindings(_ context.Context, bindings []store.WorkspaceAuthBinding, desiredServiceIDs []uuid.UUID) error {
	s.workspaceAuthPreflight = append(s.workspaceAuthPreflight, bindings...)
	s.workspaceAuthDesiredIDs = append(s.workspaceAuthDesiredIDs, desiredServiceIDs...)
	// Injected typed failures model the production PostgreSQL graph admission
	// boundary without allowing later mutation assertions to become coupled to SQL.
	if s.workspaceAuthPreflightErr != nil {
		return s.workspaceAuthPreflightErr
	}
	return nil
}

// ApplyWorkspaceAuthBindings records the atomic batch and its direct material
// so existing workspace-apply assertions observe the current persistence path.
func (s *workspaceTestStore) ApplyWorkspaceAuthBindings(_ context.Context, bindings []store.WorkspaceAuthBinding) error {
	s.workspaceAuthBindings = append(s.workspaceAuthBindings, bindings...)
	for _, binding := range bindings {
		s.upsertedSecrets = append(s.upsertedSecrets, binding.Secrets...)
	}
	return nil
}

func (s *workspaceTestStore) DeleteSecrets(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyNames []string) error {
	s.deletedSecretKeys = append(s.deletedSecretKeys, keyNames...)
	return nil
}

func (s *workspaceTestStore) DeleteSecret(ctx context.Context, bucketID uuid.UUID, serviceID uuid.UUID, keyName string) error {
	return nil
}

func (s *workspaceTestStore) GetBucketByName(ctx context.Context, name string) (*store.Bucket, error) {
	s.bucketLookupNames = append(s.bucketLookupNames, name)
	if s.bucketLookupErr != nil {
		return nil, s.bucketLookupErr
	}
	if s.bucketsByName != nil {
		if bucket := s.bucketsByName[name]; bucket != nil {
			return bucket, nil
		}
		return nil, store.ErrBucketNotFound
	}
	return &store.Bucket{ID: workspaceTestBucketID(name), Name: name, IsDefault: name == "default"}, nil
}

func workspaceTestBucketID(name string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("workspace-test-bucket:"+name))
}

func (s *workspaceTestStore) GetBucketsByNames(ctx context.Context, names []string) ([]store.Bucket, error) {
	s.bucketBatchLookupNames = append(s.bucketBatchLookupNames, append([]string(nil), names...))
	if s.bucketBatchLookupErr != nil {
		return nil, s.bucketBatchLookupErr
	}
	byName := make(map[string]store.Bucket, len(s.buckets))
	for _, bucket := range s.buckets {
		byName[bucket.Name] = bucket
	}
	for name, bucket := range s.bucketsByName {
		if bucket != nil {
			copy := *bucket
			if copy.Name == "" {
				copy.Name = name
			}
			byName[name] = copy
		}
	}
	buckets := make([]store.Bucket, 0, len(names))
	for _, name := range names {
		if bucket, ok := byName[name]; ok {
			buckets = append(buckets, bucket)
			continue
		}
		if s.bucketsByName == nil {
			buckets = append(buckets, store.Bucket{ID: uuid.New(), Name: name, IsDefault: name == "default"})
		}
	}
	return buckets, nil
}

func (s *workspaceTestStore) GetEffectiveWorkspaceExecutionPolicyOverrides(context.Context, []store.WorkspaceExecutionPolicyRef) (map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride, error) {
	return map[store.WorkspaceExecutionPolicyRef]*store.WorkspaceExecutionPolicyOverride{}, nil
}

// GetSecret keys purely by KeyName (ignoring bucketID/serviceID) since these
// tests seed one distinctly-named secret per case -- see secretsByKey, used
// by the connect client_secret bucket-ref apply tests.
func (s *workspaceTestStore) GetSecret(ctx context.Context, bucketID, serviceID uuid.UUID, keyName string) (*store.WorkspaceSecret, error) {
	s.secretLookupKeys = append(s.secretLookupKeys, keyName)
	if s.secretLookupErr != nil {
		return nil, s.secretLookupErr
	}
	if s.secretsByKey == nil {
		return nil, nil
	}
	return s.secretsByKey[keyName], nil
}

// ListBuckets returns the seeded bucket slice so GraphQL tests can exercise
// bucket selection without standing up Postgres.
func (s *workspaceTestStore) ListBuckets(ctx context.Context) ([]store.Bucket, error) {
	return s.buckets, nil
}

func (s *workspaceTestStore) ListBucketSummaries(ctx context.Context, limit, offset int) ([]store.BucketSummary, int, error) {
	total := len(s.bucketSummaries)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return s.bucketSummaries[offset:end], total, nil
}

func (s *workspaceTestStore) ListAuthorizedBucketSummaries(ctx context.Context, scope accesscontrol.AuthorizedScope, limit, offset int) ([]store.BucketSummary, int, error) {
	if scope.All {
		return s.ListBucketSummaries(ctx, limit, offset)
	}
	allowed := make(map[uuid.UUID]struct{}, len(scope.IDs))
	for _, id := range scope.IDs {
		allowed[id] = struct{}{}
	}
	filtered := make([]store.BucketSummary, 0, len(s.bucketSummaries))
	for _, summary := range s.bucketSummaries {
		if _, ok := allowed[summary.ID]; ok {
			filtered = append(filtered, summary)
		}
	}
	total := len(filtered)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return filtered[offset:end], total, nil
}

func (s *workspaceTestStore) GetBucketSummary(ctx context.Context, bucketID uuid.UUID) (*store.BucketSummary, error) {
	for _, summary := range s.bucketSummaries {
		if summary.ID == bucketID {
			return &summary, nil
		}
	}
	return nil, store.ErrBucketNotFound
}

// GetBucket enforces workspace ownership in the fixture because the GraphQL
// resolver depends on this check before reading credential state.
func (s *workspaceTestStore) GetBucket(ctx context.Context, bucketID uuid.UUID) (*store.Bucket, error) {
	if s.bucketsByID != nil {
		if bucket := s.bucketsByID[bucketID]; bucket != nil {
			return bucket, nil
		}
		return nil, store.ErrBucketNotFound
	}
	for _, bucket := range s.buckets {
		if bucket.ID == bucketID {
			return &bucket, nil
		}
	}
	return &store.Bucket{ID: bucketID, Name: "default", IsDefault: true}, nil
}

func (s *workspaceTestStore) CreateBucket(ctx context.Context, name string, isDefault bool) (*store.Bucket, error) {
	return &store.Bucket{ID: uuid.New(), Name: name, IsDefault: isDefault}, nil
}

func (s *workspaceTestStore) LinkBucketToSDK(ctx context.Context, appID, bucketID uuid.UUID) error {
	s.linkBucketCalls++
	return nil
}

func (s *workspaceTestStore) ListBucketsForAppFamily(ctx context.Context, appFamilyID uuid.UUID) ([]store.Bucket, error) {
	if s.sdkBuckets != nil {
		return s.sdkBuckets[appFamilyID], nil
	}
	return nil, nil
}

func (s *workspaceTestStore) ListAuthorizedBucketsForAppFamily(ctx context.Context, appFamilyID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]store.Bucket, error) {
	buckets, err := s.ListBucketsForAppFamily(ctx, appFamilyID)
	if err != nil || scope.All {
		return buckets, err
	}
	allowed := testUUIDSet(scope.IDs)
	var filtered []store.Bucket
	for _, bucket := range buckets {
		if _, ok := allowed[bucket.ID]; ok {
			filtered = append(filtered, bucket)
		}
	}
	return filtered, nil
}

func (s *workspaceTestStore) ListAppRuntimesForBucket(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]store.AppRuntime, int, error) {
	items, total := pageAppRuntimes(s.appRuntimesForBucket[bucketID], limit, offset)
	return items, total, nil
}

func (s *workspaceTestStore) ListAuthorizedAppRuntimesForBucket(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, limit, offset int) ([]store.AppRuntime, int, error) {
	if scope.All {
		return s.ListAppRuntimesForBucket(ctx, bucketID, limit, offset)
	}
	allowed := testUUIDSet(scope.IDs)
	var filtered []store.AppRuntime
	for _, artifact := range s.appRuntimesForBucket[bucketID] {
		if _, ok := allowed[artifact.AppID]; ok {
			filtered = append(filtered, artifact)
		}
	}
	items, total := pageAppRuntimes(filtered, limit, offset)
	return items, total, nil
}

func (s *workspaceTestStore) ListBucketValues(ctx context.Context, bucketID uuid.UUID) ([]store.BucketValue, error) {
	if s.bucketValues != nil {
		return s.bucketValues[bucketID], nil
	}
	return nil, nil
}

func (s *workspaceTestStore) GetBucketValues(ctx context.Context, bucketID, serviceID uuid.UUID, keyNames []string) ([]store.BucketValue, error) {
	return nil, nil
}

func (s *workspaceTestStore) ListBucketValuePage(ctx context.Context, bucketID uuid.UUID, limit, offset int) ([]store.BucketValue, int, error) {
	items, total := pageBucketValues(s.bucketValues[bucketID], limit, offset)
	return items, total, nil
}

func (s *workspaceTestStore) UpsertConnectConfig(ctx context.Context, cfg store.ConnectConfig) (*store.ConnectConfig, error) {
	if s.upsertConnectConfigErr != nil {
		return nil, s.upsertConnectConfigErr
	}
	cfg.ID = uuid.New()
	cfg.CreatedAt = time.Now().UTC()
	cfg.UpdatedAt = cfg.CreatedAt
	s.upsertedConnectConfigs = append(s.upsertedConnectConfigs, cfg)
	return &cfg, nil
}

// GetConnectConfig first checks explicit fixtures, then recent upserts, so read
// and write GraphQL tests can share the same lightweight store.
func (s *workspaceTestStore) GetConnectConfig(ctx context.Context, bucketID, serviceID uuid.UUID) (*store.ConnectConfig, error) {
	if s.connectConfigs != nil {
		return s.connectConfigs[bucketID.String()+":"+serviceID.String()], nil
	}
	for i := len(s.upsertedConnectConfigs) - 1; i >= 0; i-- {
		cfg := s.upsertedConnectConfigs[i]
		if cfg.BucketID == bucketID && cfg.ServiceID == serviceID {
			return &cfg, nil
		}
	}
	return nil, nil
}

// ListConnectConfigsForBucket keeps artifact readiness tests on the same
// bounded bucket read as production without requiring a database fixture.
func (s *workspaceTestStore) ListConnectConfigsForBucket(ctx context.Context, bucketID uuid.UUID) ([]store.ConnectConfig, error) {
	s.connectConfigsForBucketCalls++
	if s.connectConfigsForBucketErr != nil {
		return nil, s.connectConfigsForBucketErr
	}
	var configs []store.ConnectConfig
	for _, cfg := range s.connectConfigs {
		if cfg != nil && cfg.BucketID == bucketID {
			configs = append(configs, *cfg)
		}
	}
	return configs, nil
}

func (s *workspaceTestStore) ListConnectConfigsForService(ctx context.Context, serviceID uuid.UUID) ([]store.ConnectConfig, error) {
	return s.serviceConnectConfigs, nil
}

// ListWorkspaceConnectConfigs supplies the fixed-query GraphQL sync fixture
// without coupling unrelated tests to a real database.
func (s *workspaceTestStore) ListWorkspaceConnectConfigs(ctx context.Context) ([]store.WorkspaceConnectConfig, error) {
	return s.workspaceConnectConfigs, nil
}

// ListWorkspaceConnectProfiles supplies active profile snapshots paired with
// the workspace connect fixtures used by GraphQL sync tests.
func (s *workspaceTestStore) ListWorkspaceConnectProfiles(ctx context.Context) ([]store.WorkspaceConnectionProfile, error) {
	return s.workspaceConnectProfiles, nil
}

func (s *workspaceTestStore) GetBucketConnectSummary(ctx context.Context, bucketID uuid.UUID) (*store.BucketConnectSummary, error) {
	if s.bucketConnectSummaries != nil {
		if summary := s.bucketConnectSummaries[bucketID]; summary != nil {
			return summary, nil
		}
	}
	return &store.BucketConnectSummary{BucketID: bucketID}, nil
}

func (s *workspaceTestStore) ListBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, search string, limit, offset int) ([]store.BucketServiceSummary, int, error) {
	items, total := pageBucketServiceSummaries(filterBucketServiceSummaries(s.bucketServiceSummaries[bucketID], search), limit, offset)
	return items, total, nil
}

func (s *workspaceTestStore) ListAuthorizedBucketServiceSummaries(ctx context.Context, bucketID uuid.UUID, scope accesscontrol.AuthorizedScope, search string, limit, offset int) ([]store.BucketServiceSummary, int, error) {
	if scope.All {
		return s.ListBucketServiceSummaries(ctx, bucketID, search, limit, offset)
	}
	allowed := testUUIDSet(scope.IDs)
	var filtered []store.BucketServiceSummary
	for _, service := range s.bucketServiceSummaries[bucketID] {
		if _, ok := allowed[service.ServiceID]; ok {
			filtered = append(filtered, service)
		}
	}
	items, total := pageBucketServiceSummaries(filterBucketServiceSummaries(filtered, search), limit, offset)
	return items, total, nil
}

func testUUIDSet(ids []uuid.UUID) map[uuid.UUID]struct{} {
	set := make(map[uuid.UUID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set
}

// ListAuthConnections mirrors Store filtering inside the fixture only; the real
// Postgres implementation keeps these filters in SQL.
func (s *workspaceTestStore) ListAuthConnections(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string) ([]store.AuthConnection, error) {
	var out []store.AuthConnection
	for _, conn := range s.authConnections {
		if conn.BucketID != bucketID {
			continue
		}
		if serviceID != nil && conn.ServiceID != *serviceID {
			continue
		}
		if endUserRef != "" && conn.EndUserRef != endUserRef {
			continue
		}
		out = append(out, conn)
	}
	return out, nil
}

func (s *workspaceTestStore) ListAuthConnectionsPage(ctx context.Context, bucketID uuid.UUID, serviceID *uuid.UUID, endUserRef string, limit, offset int) ([]store.AuthConnection, int, error) {
	connections, err := s.ListAuthConnections(ctx, bucketID, serviceID, endUserRef)
	if err != nil {
		return nil, 0, err
	}
	items, total := pageAuthConnections(connections, limit, offset)
	return items, total, nil
}

// GetAuthConnectionByIDForBuckets lets GraphQL getConnection tests exercise
// opaque ID lookup without weakening the bucket ownership boundary.
func (s *workspaceTestStore) GetAuthConnectionByIDForBuckets(ctx context.Context, id uuid.UUID, bucketIDs []uuid.UUID) (*store.AuthConnection, error) {
	allowed := map[uuid.UUID]struct{}{}
	for _, bucketID := range bucketIDs {
		allowed[bucketID] = struct{}{}
	}
	for _, conn := range s.authConnections {
		if conn.ID == id {
			if _, ok := allowed[conn.BucketID]; ok {
				found := conn
				return &found, nil
			}
		}
	}
	return nil, nil
}

func (s *workspaceTestStore) GetAuthConnectionsByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]store.AuthConnection, error) {
	s.getAuthConnectionsByIDsCalls++
	allowed := testUUIDSet(ids)
	connections := make(map[uuid.UUID]store.AuthConnection, len(ids))
	for _, connection := range s.authConnections {
		if _, ok := allowed[connection.ID]; ok {
			connections[connection.ID] = connection
		}
	}
	return connections, nil
}

// DeleteAuthConnection records the deleted ID only when the bucket matches,
// matching the production cross-bucket guard.
func (s *workspaceTestStore) DeleteAuthConnection(ctx context.Context, bucketID, id uuid.UUID) error {
	for _, conn := range s.authConnections {
		if conn.BucketID == bucketID && conn.ID == id {
			s.deletedAuthConnections = append(s.deletedAuthConnections, id)
			return nil
		}
	}
	return store.ErrAuthConnectionNotFound
}

// ListConnectionResources returns only rows for the requested connection so
// GraphQL tests can assert the resolver does not broaden the lookup.
func (s *workspaceTestStore) ListConnectionResources(ctx context.Context, connectionID uuid.UUID) ([]store.ConnectionResource, error) {
	return s.connectionResources[connectionID], nil
}

// SetDefaultConnectionResource records the mutation and returns a connection-
// scoped row, matching production's ownership predicate.
func (s *workspaceTestStore) SetDefaultConnectionResource(ctx context.Context, connectionID, resourceID uuid.UUID) (*store.ConnectionResource, error) {
	for _, resource := range s.connectionResources[connectionID] {
		if resource.ID == resourceID {
			resource.IsDefault = true
			s.defaultConnectionResourceID = resourceID
			return &resource, nil
		}
	}
	return nil, errors.New("connection resource not found")
}

// CreateConnectSession captures session writes for GraphQL start-session tests
// without exposing the encrypted PKCE material to assertions.
func (s *workspaceTestStore) CreateConnectSession(ctx context.Context, session store.ConnectSession) (*store.ConnectSession, error) {
	session.ID = uuid.New()
	session.CreatedAt = time.Now().UTC()
	s.createdConnectSessions = append(s.createdConnectSessions, session)
	return &session, nil
}

func pageWorkspaceSecretMetas(items []store.WorkspaceSecretMeta, limit, offset int) ([]store.WorkspaceSecretMeta, int) {
	return pageTestItems(items, limit, offset)
}

func pageAppRuntimes(items []store.AppRuntime, limit, offset int) ([]store.AppRuntime, int) {
	return pageTestItems(items, limit, offset)
}

func pageBucketServiceSummaries(items []store.BucketServiceSummary, limit, offset int) ([]store.BucketServiceSummary, int) {
	return pageTestItems(items, limit, offset)
}

func filterBucketServiceSummaries(items []store.BucketServiceSummary, search string) []store.BucketServiceSummary {
	if strings.TrimSpace(search) == "" {
		return items
	}
	needle := strings.ToLower(strings.TrimSpace(search))
	var out []store.BucketServiceSummary
	for _, item := range items {
		if strings.Contains(strings.ToLower(item.ServiceName), needle) || strings.Contains(item.ServiceID.String(), needle) {
			out = append(out, item)
		}
	}
	return out
}

func pageBucketValues(items []store.BucketValue, limit, offset int) ([]store.BucketValue, int) {
	return pageTestItems(items, limit, offset)
}

func pageAuthConnections(items []store.AuthConnection, limit, offset int) ([]store.AuthConnection, int) {
	return pageTestItems(items, limit, offset)
}

func pageTestItems[T any](items []T, limit, offset int) ([]T, int) {
	total := len(items)
	if offset >= total {
		return nil, total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return items[offset:end], total
}
