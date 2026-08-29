package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/credentialkeys"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// connectAdminFixture groups the exact workspace identities shared by connect handler tests.
type connectAdminFixture struct {
	store     *connectAdminMockStore
	masterKey []byte
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

// connectAdminMockStore provides bounded in-memory state for current consent and connection tests.
type connectAdminMockStore struct {
	store.Store
	accountID            uuid.UUID
	workspaceID          uuid.UUID
	bucketID             uuid.UUID
	serviceID            uuid.UUID
	sourceServiceID      uuid.UUID
	sourceAuthName       string
	sourceVersion        string
	bucketErr            error
	applicationAuthType  string
	applicationAuthName  string
	applicationSecrets   []store.WorkspaceSecret
	createdSessions      []store.ConnectSession
	inputSessions        []store.ConnectInputSession
	session              *store.ConnectSession
	markedStateHash      string
	savedConnection      *store.AuthConnection
	connections          []store.AuthConnection
	deletedConnectionID  uuid.UUID
	deleteErr            error
	latestVersion        string
	exactVersions        map[uuid.UUID]string
	reconciledResources  []store.ConnectionResource
	callbackPersistErr   error
	callbackPersistCalls int
	appRuntimes          map[uuid.UUID]*store.AppRuntime
	connectBranding      store.ConnectBranding
}

// ResolveGenerationServiceIDsByKeys models the canonical set-based source identity resolver.
func (s *connectAdminMockStore) ResolveGenerationServiceIDsByKeys(_ context.Context, keys []string) (map[string]uuid.UUID, error) {
	resolved := map[string]uuid.UUID{}
	// Only the explicit source fixture is admitted; unknown keys remain unresolved.
	for _, key := range keys {
		if key == "gmail" && s.sourceServiceID != uuid.Nil {
			resolved[key] = s.sourceServiceID
		}
	}
	return resolved, nil
}

// ListAuthorizedWorkspaceServices projects only exact IDs supplied by the connect source resolver.
func (s *connectAdminMockStore) ListAuthorizedWorkspaceServices(_ context.Context, scope accesscontrol.AuthorizedScope, _ []string) ([]store.WorkspaceService, error) {
	// The fixture never grants a broad listing because source-only membership must stay explicit.
	if scope.All || len(scope.IDs) != 1 || scope.IDs[0] != s.sourceServiceID {
		return nil, nil
	}
	return []store.WorkspaceService{{ServiceID: s.sourceServiceID, ServiceSlug: "gmail", Version: s.sourceVersion}}, nil
}

// ListGenerationAuthContracts returns the bounded source auth projection used by app planning.
func (s *connectAdminMockStore) ListGenerationAuthContracts(_ context.Context, selections []store.GenerationAuthSelection, _ bool) ([]store.GenerationAuthContract, error) {
	// One exact source/version request is required before auth metadata is exposed.
	if len(selections) != 1 || selections[0].ServiceID != s.sourceServiceID || selections[0].Version != s.sourceVersion {
		return nil, store.ErrGenerationContractPinUnavailable
	}
	return []store.GenerationAuthContract{{GenerationAuthSelection: selections[0], AuthConfigs: fusedobject.AuthConfigs{{Type: "oauth2", Name: s.sourceAuthName}}}}, nil
}

// ListGenerationContractBindings is unused by connect tests but completes the shared planner store contract.
func (s *connectAdminMockStore) ListGenerationContractBindings(context.Context, []models.ServiceVersionRef, bool) ([]models.SDKContractBinding, error) {
	return nil, nil
}

// ValidateGenerationSelections is unused because standalone auth refs never select source operations.
func (s *connectAdminMockStore) ValidateGenerationSelections(context.Context, []models.SDKSelection, bool) error {
	return nil
}

// GetConnectBranding returns configured branding or the compiled fallback used by hosted-connect tests.
func (s *connectAdminMockStore) GetConnectBranding(_ context.Context) (store.ConnectBranding, error) {
	// Empty fixture branding exercises the same fallback as an unconfigured workspace.
	if s.connectBranding.DisplayName == "" {
		return store.DefaultConnectBranding(), nil
	}
	return s.connectBranding, nil
}

// GetAppRuntime resolves exact optional app attribution for consent tests.
func (s *connectAdminMockStore) GetAppRuntime(_ context.Context, id uuid.UUID) (*store.AppRuntime, error) {
	// Missing app identity must fail rather than select another runtime.
	if scope := s.appRuntimes[id]; scope != nil {
		return scope, nil
	}
	return nil, store.ErrAppRuntimeNotFound
}

// GetApp projects the exact immutable version identity used to authorize optional SDK audit attribution.
func (s *connectAdminMockStore) GetApp(_ context.Context, id uuid.UUID) (*store.App, error) {
	scope := s.appRuntimes[id]
	// Unknown versions remain opaque to attribution callers.
	if scope == nil {
		return nil, store.ErrAppNotFound
	}
	return &store.App{AppID: id, AppFamilyID: id, AccountID: scope.AccountID}, nil
}

// GetAppFamily projects SDK/MCP kind and account ownership for attribution tests.
func (s *connectAdminMockStore) GetAppFamily(_ context.Context, familyID uuid.UUID) (*store.AppFamily, error) {
	scope := s.appRuntimes[familyID]
	// The fixture uses the Version ID as its family ID to keep authorization setup bounded.
	if scope == nil {
		return nil, store.ErrAppFamilyNotFound
	}
	return &store.AppFamily{AppFamilyID: familyID, AccountID: scope.AccountID, Kind: scope.Kind}, nil
}

// newConnectAdminFixture constructs one isolated workspace, bucket, and service test boundary.
func newConnectAdminFixture() connectAdminFixture {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	return connectAdminFixture{
		masterKey: []byte("12345678901234567890123456789012"), bucketID: bucketID, serviceID: serviceID,
		store: &connectAdminMockStore{accountID: uuid.New(), workspaceID: workspaceID, bucketID: bucketID, serviceID: serviceID},
	}
}

// buildConnectAdminRouter mounts current workspace credential and consent routes for handler tests.
func buildConnectAdminRouter(s store.Store, accountID uuid.UUID, masterKey []byte) http.Handler {
	router := newControlTestRouter(accountID)
	router.Mount("/workspace", WorkspaceHandler(s, &mockVerifier{}, masterKey, s, "https://engine.example.com/workspace/connect/callback"))
	return router
}

// GetAccountByAPIKey returns the fixture actor without introducing another authentication path.
func (s *connectAdminMockStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

// VerifyWorkspaceOwner admits the isolated fixture actor as its workspace owner.
func (s *connectAdminMockStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

// GetBucket enforces the fixture's injected bucket lookup outcome.
func (s *connectAdminMockStore) GetBucket(context.Context, uuid.UUID) (*store.Bucket, error) {
	// Tests inject authoritative absence before any credential or connection mutation.
	if s.bucketErr != nil {
		return nil, s.bucketErr
	}
	return &store.Bucket{ID: s.bucketID, Name: "production"}, nil
}

// GetFirstCompleteSecretSet returns only the exact application pair requested by the shared resolver.
func (s *connectAdminMockStore) GetFirstCompleteSecretSet(_ context.Context, bucketID, serviceID uuid.UUID, alternatives []store.SecretKeyAlternative) ([]store.WorkspaceSecret, error) {
	// The fixture admits one exact two-key alternative and treats every other shape as unavailable.
	if !connectSecretFixtureShapeMatches(s, bucketID, serviceID, alternatives) {
		return nil, nil
	}
	required := alternatives[0].Required
	storedKeys, storageServiceID, ok := connectSecretFixtureStorageIdentity(s, serviceID, alternatives[0])
	// Invalid or mismatched source metadata must make the test store fail closed like PostgreSQL.
	if !ok {
		return nil, nil
	}
	// Both deterministic names must be present before the family is considered complete.
	if !connectSecretFixtureRowsMatch(s.applicationSecrets, storedKeys, storageServiceID) {
		return nil, nil
	}
	secrets := append([]store.WorkspaceSecret(nil), s.applicationSecrets...)
	// The production query aliases source rows to target keys so decryption remains target-contract shaped.
	for index := range secrets {
		secrets[index].KeyName = required[index]
	}
	return secrets, nil
}

// connectSecretFixtureShapeMatches bounds the mock to the resolver's one-pair query contract.
func connectSecretFixtureShapeMatches(s *connectAdminMockStore, bucketID, serviceID uuid.UUID, alternatives []store.SecretKeyAlternative) bool {
	return bucketID == s.bucketID && serviceID == s.serviceID && len(alternatives) == 1 && len(alternatives[0].Required) == 2 && len(s.applicationSecrets) == 2
}

// connectSecretFixtureStorageIdentity resolves the direct or referenced storage tuple expected from PostgreSQL.
func connectSecretFixtureStorageIdentity(s *connectAdminMockStore, targetServiceID uuid.UUID, alternative store.SecretKeyAlternative) ([]string, uuid.UUID, bool) {
	// A missing source means the target service's deterministic keys are read directly.
	if alternative.SourceServiceID == uuid.Nil {
		return alternative.Required, targetServiceID, true
	}
	clientIDKey, clientSecretKey, ok := credentialkeys.OAuthApplication(alternative.SourceAuthName)
	knownService := alternative.SourceServiceID == s.serviceID || alternative.SourceServiceID == s.sourceServiceID
	// References must identify one admitted service and the exact canonical family under test.
	if !ok || !knownService || alternative.SourceAuthType != s.applicationAuthType {
		return nil, uuid.Nil, false
	}
	return []string{clientIDKey, clientSecretKey}, alternative.SourceServiceID, true
}

// connectSecretFixtureRowsMatch proves both stored rows belong to the expected service and deterministic pair.
func connectSecretFixtureRowsMatch(secrets []store.WorkspaceSecret, storedKeys []string, storageServiceID uuid.UUID) bool {
	return slices.Contains(storedKeys, secrets[0].KeyName) && slices.Contains(storedKeys, secrets[1].KeyName) && secrets[0].ServiceID == storageServiceID && secrets[1].ServiceID == storageServiceID
}

// GetLatestWorkspaceServiceVersionByWorkspace supplies the exact activated version for start tests.
func (s *connectAdminMockStore) GetLatestWorkspaceServiceVersionByWorkspace(context.Context, uuid.UUID) (string, error) {
	// An explicit version fixture overrides the stable default.
	if s.latestVersion != "" {
		return s.latestVersion, nil
	}
	return "2026-07-01", nil
}

// GetWorkspaceServiceVersion resolves callback sessions against their pinned immutable version.
func (s *connectAdminMockStore) GetWorkspaceServiceVersion(_ context.Context, serviceID, serviceVersionID uuid.UUID) (*store.WorkspaceServiceVersion, error) {
	// Cross-service or absent version identities cannot float to another contract.
	if serviceID != s.serviceID || serviceVersionID == uuid.Nil {
		return nil, store.ErrWorkspaceServiceVersionNotFound
	}
	version := s.latestVersion
	// Exact per-version fixtures take precedence over the general test version.
	if exact, ok := s.exactVersions[serviceVersionID]; ok {
		version = exact
	}
	// The default keeps unrelated tests pinned without needing repeated setup.
	if version == "" {
		version = "2026-07-01"
	}
	return &store.WorkspaceServiceVersion{ServiceID: serviceID, ServiceVersionID: serviceVersionID, Version: version, Status: "public"}, nil
}

// CreateConnectSession records one provider callback session for subsequent exact lookup.
func (s *connectAdminMockStore) CreateConnectSession(_ context.Context, session store.ConnectSession) (*store.ConnectSession, error) {
	// Direct callback fixtures may omit identity, while production-created sessions preserve their preallocated browser correlation.
	if session.ID == uuid.Nil {
		session.ID = uuid.New()
	}
	session.CreatedAt = time.Now().UTC()
	s.createdSessions = append(s.createdSessions, session)
	return &session, nil
}

// GetConnectSessionByStateHash returns only a session with the exact stored state hash.
func (s *connectAdminMockStore) GetConnectSessionByStateHash(_ context.Context, stateHash string) (*store.ConnectSession, error) {
	// An explicitly injected callback session has priority for failure-state tests.
	if s.session != nil && s.session.StateHash == stateHash {
		return s.session, nil
	}
	for _, session := range s.createdSessions {
		// State hashes are opaque exact identities and never support partial matching.
		if session.StateHash == stateHash {
			found := session
			return &found, nil
		}
	}
	return nil, nil
}

// MarkConnectSessionUsed enforces one-time callback consumption in the fixture.
func (s *connectAdminMockStore) MarkConnectSessionUsed(_ context.Context, stateHash string, usedAt time.Time) error {
	// Missing, mismatched, or previously used state cannot be consumed again.
	if s.session == nil || s.session.StateHash != stateHash || s.session.UsedAt != nil {
		return store.ErrConnectSessionUnavailable
	}
	s.markedStateHash = stateHash
	s.session.UsedAt = &usedAt
	return nil
}

// UpsertAuthConnection records the latest encrypted connected-user grant.
func (s *connectAdminMockStore) UpsertAuthConnection(_ context.Context, conn store.AuthConnection) (*store.AuthConnection, error) {
	conn.ID = uuid.New()
	conn.CreatedAt = time.Now().UTC()
	conn.UpdatedAt = conn.CreatedAt
	s.savedConnection = &conn
	return &conn, nil
}

// UpsertAuthConnectionAndReconcileResources models the production all-or-none callback commit.
func (s *connectAdminMockStore) UpsertAuthConnectionAndReconcileResources(_ context.Context, conn store.AuthConnection, resources []store.ConnectionResource) (*store.AuthConnection, []store.ConnectionResource, error) {
	s.callbackPersistCalls++
	// Injected failure occurs before fixture state changes to preserve the prior grant.
	if s.callbackPersistErr != nil {
		return nil, nil, s.callbackPersistErr
	}
	// Reconnect replaces only the exact bucket/service/user/auth tuple.
	if s.savedConnection != nil && s.savedConnection.BucketID == conn.BucketID && s.savedConnection.ServiceID == conn.ServiceID && s.savedConnection.EndUserRef == conn.EndUserRef && s.savedConnection.AuthName == conn.AuthName {
		conn.ID = s.savedConnection.ID
	} else {
		conn.ID = uuid.New()
	}
	conn.UpdatedAt = time.Now().UTC()
	// First persistence owns the immutable creation timestamp.
	if conn.CreatedAt.IsZero() {
		conn.CreatedAt = conn.UpdatedAt
	}
	stored := append([]store.ConnectionResource(nil), resources...)
	for index := range stored {
		stored[index].ConnectionID = conn.ID
	}
	s.savedConnection = &conn
	s.reconciledResources = stored
	return &conn, append([]store.ConnectionResource(nil), stored...), nil
}

// ReconcileConnectionResources captures the set submitted by rediscovery tests.
func (s *connectAdminMockStore) ReconcileConnectionResources(_ context.Context, _ uuid.UUID, resources []store.ConnectionResource) ([]store.ConnectionResource, error) {
	s.reconciledResources = append([]store.ConnectionResource(nil), resources...)
	return s.reconciledResources, nil
}

// ListAuthConnections returns the bounded fixture connection set.
func (s *connectAdminMockStore) ListAuthConnections(context.Context, uuid.UUID, *uuid.UUID, string) ([]store.AuthConnection, error) {
	return append([]store.AuthConnection(nil), s.connections...), nil
}

// GetAuthConnection resolves only the exact connected-user identity requested by runtime.
func (s *connectAdminMockStore) GetAuthConnection(_ context.Context, bucketID, serviceID uuid.UUID, endUserRef, authName string) (*store.AuthConnection, error) {
	// Partial identity matches cannot select a sibling user's grant.
	if s.savedConnection != nil && s.savedConnection.BucketID == bucketID && s.savedConnection.ServiceID == serviceID && s.savedConnection.EndUserRef == endUserRef && s.savedConnection.AuthName == authName {
		return s.savedConnection, nil
	}
	return nil, store.ErrAuthConnectionNotFound
}

// TouchAuthConnectionLastUsed keeps fixture dispatch bookkeeping side-effect free.
func (s *connectAdminMockStore) TouchAuthConnectionLastUsed(context.Context, uuid.UUID, time.Time) error {
	return nil
}

// ListConnectionResources returns an isolated copy of reconciled provider resources.
func (s *connectAdminMockStore) ListConnectionResources(context.Context, uuid.UUID) ([]store.ConnectionResource, error) {
	return append([]store.ConnectionResource(nil), s.reconciledResources...), nil
}

// DeleteAuthConnection records an exact grant deletion or its injected failure.
func (s *connectAdminMockStore) DeleteAuthConnection(_ context.Context, _ uuid.UUID, id uuid.UUID) error {
	// Injected repository outcomes must occur before fixture mutation.
	if s.deleteErr != nil {
		return s.deleteErr
	}
	// A zero identity cannot prove an exact deletion target.
	if id == uuid.Nil {
		return errors.New("missing id")
	}
	s.deletedConnectionID = id
	return nil
}
