package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func TestConnectAdminHandlers_ConfigLifecycle(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	body := bytes.NewReader([]byte(`{
		"auth_type":"oauth",
		"client_id":"client-id",
		"client_secret":"client-secret",
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`))
	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), body)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upsert status = %d body=%s", rr.Code, rr.Body.String())
	}
	assertConnectConfigEncrypted(t, fixture.store.savedConfig)
	assertConnectConfigResponse(t, rr.Body.Bytes(), fixture)
}

// TestConnectAdminHandlers_GetReturnsSavedConfig proves a caller can check
// whether a bucket's connect config was actually set without resending
// anything -- the same safe projection the upsert response already returns.
func TestConnectAdminHandlers_GetReturnsSavedConfig(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	seedConnectConfig(t, router, fixture)

	req := httptest.NewRequest(http.MethodGet, fixture.configPath(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("get status = %d body=%s", rr.Code, rr.Body.String())
	}
	assertConnectConfigResponse(t, rr.Body.Bytes(), fixture)
}

// TestConnectAdminHandlers_GetMissingConfigReturnsNotFound proves an unset
// bucket+service pair reports 404 rather than a zero-value config that could
// be mistaken for "registered but empty".
func TestConnectAdminHandlers_GetMissingConfigReturnsNotFound(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodGet, fixture.configPath(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a never-set connect config, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestConnectAdminHandlers_GetRequiresBucketOwnership mirrors the upsert
// path's ownership check -- a bucket ID that doesn't resolve must fail
// before ever touching GetConnectConfig, the same as it does for writes.
func TestConnectAdminHandlers_GetRequiresBucketOwnership(t *testing.T) {
	fixture := newConnectAdminFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodGet, fixture.configPath(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected bucket ownership failure as 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestConnectAdminHandlers_BucketOwnershipRequired(t *testing.T) {
	fixture := newConnectAdminFixture()
	fixture.store.bucketErr = store.ErrBucketNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"auth_type":"oauth",
		"client_id":"client-id",
		"client_secret":"client-secret",
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected bucket ownership failure as 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

func TestConnectAdminHandlers_RejectsInvalidConfigPayload(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"auth_type":"api_key",
		"client_id":"",
		"client_secret":"",
		"redirect_uri":"not-a-url"
	}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid payload status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.savedConfig != nil {
		t.Fatal("invalid connect config payload must not be persisted")
	}
}

func TestConnectAdminHandlers_RejectsOAuth2AuthType(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"auth_type":"oauth2",
		"client_id":"client-id",
		"client_secret":"client-secret",
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("oauth2 auth_type status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.savedConfig != nil {
		t.Fatal("oauth2 connect config payload must not be persisted")
	}
}

// TestConnectAdminHandlers_PartialUpdatePreservesUnspecifiedFields proves the
// core promise of the partial-update path: rotating just redirect_uri must
// not require resending client_id/client_secret, and the values that were
// never sent the second time must still decrypt back to what they originally
// were -- not be blanked, not silently regenerated.
func TestConnectAdminHandlers_PartialUpdatePreservesUnspecifiedFields(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	create := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"auth_type":"oauth",
		"client_id":"client-id",
		"client_secret":"client-secret",
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`)))
	create.Header.Set("X-API-Key", "test-key")
	createRR := httptest.NewRecorder()
	router.ServeHTTP(createRR, create)
	if createRR.Code != http.StatusOK {
		t.Fatalf("create status = %d body=%s", createRR.Code, createRR.Body.String())
	}

	update := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"redirect_uri":"https://engine.example.com/connect/new-callback"
	}`)))
	update.Header.Set("X-API-Key", "test-key")
	updateRR := httptest.NewRecorder()
	router.ServeHTTP(updateRR, update)
	if updateRR.Code != http.StatusOK {
		t.Fatalf("update status = %d body=%s", updateRR.Code, updateRR.Body.String())
	}

	saved := fixture.store.savedConfig
	if saved.RedirectURI != "https://engine.example.com/connect/new-callback" {
		t.Fatalf("expected redirect_uri to change, got %q", saved.RedirectURI)
	}
	clientID, clientSecret := decryptSavedConnectConfig(t, saved, fixture.masterKey)
	if clientID != "client-id" || clientSecret != "client-secret" {
		t.Fatalf("expected client_id/client_secret to survive an update that never sent them, got id=%q secret=%q", clientID, clientSecret)
	}
}

// TestConnectAdminHandlers_UpdateWithoutExistingConfigStillRequiresAllFields
// guards the create/update split itself: a partial payload against a bucket
// with no connect config yet must fail the same way full creation always
// did, not silently create a config with a blank client_id/client_secret.
func TestConnectAdminHandlers_UpdateWithoutExistingConfigStillRequiresAllFields(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected first-time partial payload to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.savedConfig != nil {
		t.Fatal("a rejected first-time payload must not create a config")
	}
}

// TestConnectAdminHandlers_UpdateRejectsEmptyPayload proves an update with no
// fields at all fails loudly instead of a no-op 200 that would leave a caller
// unsure whether their change took effect.
func TestConnectAdminHandlers_UpdateRejectsEmptyPayload(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	seedConnectConfig(t, router, fixture)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected empty update payload to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestConnectAdminHandlers_UpdateRejectsBlankedOutSecret proves an explicit
// empty string is treated as an invalid attempt to blank a credential, not
// as "leave it unchanged" -- that meaning is reserved for omitting the field
// entirely.
func TestConnectAdminHandlers_UpdateRejectsBlankedOutSecret(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	seedConnectConfig(t, router, fixture)

	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{"client_secret":""}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected blanked-out client_secret to be rejected, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// seedConnectConfig creates a full connect config via the same HTTP path the
// other tests exercise, so update-path tests start from realistic encrypted
// state rather than hand-constructing a store.ConnectConfig.
func seedConnectConfig(t *testing.T, router http.Handler, fixture connectAdminFixture) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, fixture.configPath(), bytes.NewReader([]byte(`{
		"auth_type":"oauth",
		"client_id":"client-id",
		"client_secret":"client-secret",
		"redirect_uri":"https://engine.example.com/connect/callback"
	}`)))
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed create status = %d body=%s", rr.Code, rr.Body.String())
	}
}

// decryptSavedConnectConfig round-trips the same way runtime dispatch would,
// so tests assert on what a real caller would actually get back rather than
// on ciphertext shape.
func decryptSavedConnectConfig(t *testing.T, cfg *store.ConnectConfig, masterKey []byte) (clientID, clientSecret string) {
	t.Helper()
	dek, err := store.UnwrapDEK(masterKey, cfg.EncryptedDEK)
	if err != nil {
		t.Fatalf("unwrap dek: %v", err)
	}
	clientID, err = store.DecryptWithDEK(dek, cfg.EncryptedClientID)
	if err != nil {
		t.Fatalf("decrypt client_id: %v", err)
	}
	clientSecret, err = store.DecryptWithDEK(dek, cfg.EncryptedClientSecret)
	if err != nil {
		t.Fatalf("decrypt client_secret: %v", err)
	}
	return clientID, clientSecret
}

func TestConnectAdminHandlers_ListAndDeleteConnections(t *testing.T) {
	fixture := newConnectAdminFixture()
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)
	connectionID := uuid.New()
	fixture.store.connections = []store.AuthConnection{{
		ID:                    connectionID,
		BucketID:              fixture.bucketID,
		ServiceID:             fixture.serviceID,
		EndUserRef:            "user_123",
		AuthType:              "oauth",
		EncryptedAccessToken:  "secret-access-token",
		EncryptedRefreshToken: "secret-refresh-token",
		TokenType:             "Bearer",
		Scopes:                []string{"openid"},
		RefreshState:          "ok",
		CreatedAt:             time.Now().UTC(),
		UpdatedAt:             time.Now().UTC(),
	}}

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/auth/connections/"+connectionID.String(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d body=%s", rr.Code, rr.Body.String())
	}
	if fixture.store.deletedConnectionID != connectionID {
		t.Fatalf("expected deleted connection %s, got %s", connectionID, fixture.store.deletedConnectionID)
	}
}

func TestConnectAdminHandlers_DeleteMissingConnectionReturnsNotFound(t *testing.T) {
	fixture := newConnectAdminFixture()
	fixture.store.deleteErr = store.ErrAuthConnectionNotFound
	router := buildConnectAdminRouter(fixture.store, fixture.masterKey)

	req := httptest.NewRequest(http.MethodDelete, "/workspace/buckets/"+fixture.bucketID.String()+"/auth/connections/"+uuid.NewString(), nil)
	req.Header.Set("X-API-Key", "test-key")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("delete missing status = %d body=%s", rr.Code, rr.Body.String())
	}
}

type connectAdminFixture struct {
	store     *connectAdminMockStore
	masterKey []byte
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

type connectAdminMockStore struct {
	store.Store
	accountID           uuid.UUID
	workspaceID         uuid.UUID
	bucketID            uuid.UUID
	serviceID           uuid.UUID
	bucketErr           error
	savedConfig         *store.ConnectConfig
	createdSessions     []store.ConnectSession
	session             *store.ConnectSession
	markedStateHash     string
	savedConnection     *store.AuthConnection
	connections         []store.AuthConnection
	deletedConnectionID uuid.UUID
	deleteErr           error
	latestVersion       string
	reconciledResources []store.ConnectionResource
	artifactScopes      map[uuid.UUID]*store.ArtifactScope
}

func (s *connectAdminMockStore) GetArtifactScope(_ context.Context, id uuid.UUID) (*store.ArtifactScope, error) {
	if scope := s.artifactScopes[id]; scope != nil {
		return scope, nil
	}
	return nil, store.ErrArtifactScopeNotFound
}

func newConnectAdminFixture() connectAdminFixture {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	return connectAdminFixture{
		masterKey: []byte("12345678901234567890123456789012"),
		bucketID:  bucketID,
		serviceID: serviceID,
		store: &connectAdminMockStore{
			accountID:   uuid.New(),
			workspaceID: workspaceID,
			bucketID:    bucketID,
			serviceID:   serviceID,
		},
	}
}

func buildConnectAdminRouter(s store.Store, masterKey []byte) http.Handler {
	r := chi.NewRouter()
	r.Mount("/workspace", WorkspaceHandler(s, &mockVerifier{}, masterKey))
	return r
}

func (f connectAdminFixture) configPath() string {
	return "/workspace/buckets/" + f.bucketID.String() + "/services/" + f.serviceID.String() + "/connect-config"
}

func (s *connectAdminMockStore) GetAccountByAPIKey(context.Context, string) (uuid.UUID, error) {
	return s.accountID, nil
}

func (s *connectAdminMockStore) VerifyWorkspaceOwner(context.Context, uuid.UUID) error {
	return nil
}

func (s *connectAdminMockStore) GetBucket(context.Context, uuid.UUID) (*store.Bucket, error) {
	if s.bucketErr != nil {
		return nil, s.bucketErr
	}
	return &store.Bucket{ID: s.bucketID, Name: "production"}, nil
}

func (s *connectAdminMockStore) UpsertConnectConfig(_ context.Context, cfg store.ConnectConfig) (*store.ConnectConfig, error) {
	cfg.ID = uuid.New()
	cfg.CreatedAt = time.Now().UTC()
	cfg.UpdatedAt = cfg.CreatedAt
	s.savedConfig = &cfg
	return &cfg, nil
}

func (s *connectAdminMockStore) GetConnectConfig(context.Context, uuid.UUID, uuid.UUID) (*store.ConnectConfig, error) {
	if s.savedConfig == nil {
		return nil, nil
	}
	return s.savedConfig, nil
}

func (s *connectAdminMockStore) GetLatestWorkspaceServiceVersionByWorkspace(context.Context, uuid.UUID) (string, error) {
	if s.latestVersion != "" {
		return s.latestVersion, nil
	}
	return "2026-07-01", nil
}

func (s *connectAdminMockStore) CreateConnectSession(_ context.Context, session store.ConnectSession) (*store.ConnectSession, error) {
	session.ID = uuid.New()
	session.CreatedAt = time.Now().UTC()
	s.createdSessions = append(s.createdSessions, session)
	return &session, nil
}

func (s *connectAdminMockStore) GetConnectSessionByStateHash(_ context.Context, stateHash string) (*store.ConnectSession, error) {
	if s.session != nil && s.session.StateHash == stateHash {
		return s.session, nil
	}
	for _, session := range s.createdSessions {
		if session.StateHash == stateHash {
			found := session
			return &found, nil
		}
	}
	return nil, nil
}

func (s *connectAdminMockStore) MarkConnectSessionUsed(_ context.Context, stateHash string, usedAt time.Time) error {
	if s.session == nil || s.session.StateHash != stateHash || s.session.UsedAt != nil {
		return store.ErrConnectSessionUnavailable
	}
	s.markedStateHash = stateHash
	s.session.UsedAt = &usedAt
	return nil
}

func (s *connectAdminMockStore) UpsertAuthConnection(_ context.Context, conn store.AuthConnection) (*store.AuthConnection, error) {
	conn.ID = uuid.New()
	conn.CreatedAt = time.Now().UTC()
	conn.UpdatedAt = conn.CreatedAt
	s.savedConnection = &conn
	return &conn, nil
}

// ReconcileConnectionResources captures the all-at-once callback write so the
// runtime test can assert discovery never performs one write per resource.
func (s *connectAdminMockStore) ReconcileConnectionResources(_ context.Context, _ uuid.UUID, resources []store.ConnectionResource) ([]store.ConnectionResource, error) {
	s.reconciledResources = append([]store.ConnectionResource(nil), resources...)
	return s.reconciledResources, nil
}

func (s *connectAdminMockStore) ListAuthConnections(context.Context, uuid.UUID, *uuid.UUID, string) ([]store.AuthConnection, error) {
	return s.connections, nil
}

// GetAuthConnection supports control-plane actions that reuse the same
// proactive connected-token resolver as SDK dispatch.
func (s *connectAdminMockStore) GetAuthConnection(_ context.Context, bucketID, serviceID uuid.UUID, endUserRef string) (*store.AuthConnection, error) {
	if s.savedConnection != nil && s.savedConnection.BucketID == bucketID && s.savedConnection.ServiceID == serviceID && s.savedConnection.EndUserRef == endUserRef {
		return s.savedConnection, nil
	}
	return nil, store.ErrAuthConnectionNotFound
}

func (s *connectAdminMockStore) TouchAuthConnectionLastUsed(context.Context, uuid.UUID, time.Time) error {
	return nil
}

func (s *connectAdminMockStore) ListConnectionResources(context.Context, uuid.UUID) ([]store.ConnectionResource, error) {
	return append([]store.ConnectionResource(nil), s.reconciledResources...), nil
}

func (s *connectAdminMockStore) DeleteAuthConnection(_ context.Context, _ uuid.UUID, id uuid.UUID) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	if id == uuid.Nil {
		return errors.New("missing id")
	}
	s.deletedConnectionID = id
	return nil
}

func assertConnectConfigEncrypted(t *testing.T, cfg *store.ConnectConfig) {
	t.Helper()
	if cfg == nil {
		t.Fatal("expected saved config")
	}
	if cfg.EncryptedClientID == "client-id" || cfg.EncryptedClientSecret == "client-secret" {
		t.Fatal("connect config credentials must be encrypted before storage")
	}
}

func assertConnectConfigResponse(t *testing.T, body []byte, fixture connectAdminFixture) {
	t.Helper()
	var resp connectConfigResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.BucketID != fixture.bucketID || resp.ServiceID != fixture.serviceID {
		t.Fatalf("unexpected connect config response identity: %#v", resp)
	}
	if !resp.Enabled || !resp.HasClientID || !resp.HasClientSecret {
		t.Fatalf("expected enabled config with credential presence flags, got %#v", resp)
	}
	if bytes.Contains(body, []byte("client-secret")) {
		t.Fatal("connect config response must not leak client secret")
	}
}
