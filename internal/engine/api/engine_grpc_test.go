package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestEngineGRPCStartConnectSessionCreatesAuthorizationURL covers the
// generated-SDK path, proving it shares the same Engine-owned session creation
// as REST/GraphQL instead of minting provider URLs client-side.
func TestEngineGRPCStartConnectSessionCreatesAuthorizationURL(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	appID := attachConnectTestArtifact(&fixture)
	runtimeStore := &grpcRuntimeStore{
		Store:     fixture.store,
		accountID: fixture.store.accountID,
		appID:     appID,
		scope:     fixture.store.appRuntimes[appID],
	}
	// configStore/natsClient are nil: this test only exercises StartConnectSession,
	// which (like GetConnection/ListConnectionResources) never touches the
	// webhook-only fields SubscribeWebhooks added to EngineGRPCServer.
	srv := NewEngineGRPCServer(runtimeStore, fixture.verifier, fixture.masterKey, nil, nil, auth.NewTokenValidator(runtimeStore))
	ctx := grpcTestContext(appID)

	resp, err := srv.StartConnectSession(ctx, &enginev1.StartConnectSessionRequest{
		BucketId:       fixture.bucketID.String(),
		ServiceId:      fixture.serviceID.String(),
		EndUserRef:     "user_123",
		ReturnUrl:      "https://app.example.com/oauth/done",
		CreatedByAppId: appID.String(),
		Scopes:         []string{"openid"},
	})
	if err != nil {
		t.Fatalf("StartConnectSession() error = %v", err)
	}
	if resp.GetAuthorizeUrl() == "" || resp.GetExpiresAt() == "" {
		t.Fatalf("expected authorize URL and expiry, got %#v", resp)
	}
	if len(fixture.store.createdSessions) != 1 {
		t.Fatalf("expected one stored session, got %#v", fixture.store.createdSessions)
	}
	session := fixture.store.createdSessions[0]
	if session.EndUserRef != "user_123" || session.ReturnURL != "https://app.example.com/oauth/done" {
		t.Fatalf("unexpected stored session: %#v", session)
	}
	if strings.Join(session.RequestedScopes, " ") != "openid" || !strings.Contains(resp.GetAuthorizeUrl(), "scope=openid") {
		t.Fatalf("expected narrowed scopes in session and URL: session=%#v url=%s", session.RequestedScopes, resp.GetAuthorizeUrl())
	}
	if strings.Contains(resp.GetAuthorizeUrl(), "client-secret") {
		t.Fatalf("authorize_url must not contain client secret: %s", resp.GetAuthorizeUrl())
	}
}

// TestEngineGRPCGetConnectionReturnsMetadataOnly verifies the callback
// read-back RPC exposes bucket-scoped connection metadata, not encrypted token
// columns or decrypted provider credentials.
func TestEngineGRPCGetConnectionReturnsMetadataOnly(t *testing.T) {
	workspaceID := uuid.New()
	bucketID := uuid.New()
	serviceID := uuid.New()
	connectionID := uuid.New()
	now := time.Now().UTC()
	expiresAt := now.Add(time.Hour)
	s := &workspaceTestStore{
		accountID:   uuid.New(),
		workspaceID: workspaceID,
		buckets: []store.Bucket{{
			ID: bucketID, Name: "github", IsDefault: true,
			CreatedAt: now, UpdatedAt: now,
		}},
		authConnections: []store.AuthConnection{{
			ID: connectionID, BucketID: bucketID, ServiceID: serviceID,
			EndUserRef: "user_123", AuthType: "oauth", TokenType: "bearer", Scopes: []string{"user:email"},
			ScopeSource: "provider", ExpiresAt: &expiresAt, RefreshState: "ok", CreatedAt: now, UpdatedAt: now,
			EncryptedAccessToken: "encrypted-access-token",
		}},
	}
	appID := uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{ServiceID: serviceID}})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	runtimeStore := &grpcRuntimeStore{
		Store:     s,
		accountID: s.accountID,
		appID:     appID,
		scope: &store.AppRuntime{
			AccountID: s.accountID, AppID: appID, BucketID: bucketID, Selections: selections,
		},
	}
	// configStore/natsClient are nil -- see the identical note above; this
	// test only exercises GetConnection.
	srv := NewEngineGRPCServer(runtimeStore, &mockVerifier{}, []byte("12345678901234567890123456789012"), nil, nil, auth.NewTokenValidator(runtimeStore))

	resp, err := srv.GetConnection(grpcTestContext(appID), &enginev1.GetConnectionRequest{ConnectionId: connectionID.String()})
	if err != nil {
		t.Fatalf("GetConnection() error = %v", err)
	}
	if !resp.GetFound() || resp.GetConnection().GetEndUserRef() != "user_123" {
		t.Fatalf("unexpected connection response: %#v", resp)
	}
	if strings.Contains(resp.String(), "encrypted-access-token") {
		t.Fatalf("connection response leaked encrypted token material: %s", resp.String())
	}
	controlCredentialContext := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "fused_control_credential",
		"x-app-id", appID.String(),
	))
	_, err = srv.GetConnection(controlCredentialContext, &enginev1.GetConnectionRequest{ConnectionId: connectionID.String()})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("control credential error = %v, want Unauthenticated", err)
	}

	otherSelections, err := json.Marshal([]models.SDKSelection{{ServiceID: uuid.New()}})
	if err != nil {
		t.Fatalf("marshal other selections: %v", err)
	}
	runtimeStore.scope.Selections = otherSelections
	resp, err = srv.GetConnection(grpcTestContext(appID), &enginev1.GetConnectionRequest{ConnectionId: connectionID.String()})
	if err != nil || resp.GetFound() {
		t.Fatalf("cross-service GetConnection = (%#v, %v), want hidden", resp, err)
	}
	_, err = srv.ListConnectionResources(grpcTestContext(appID), &enginev1.ListConnectionResourcesRequest{ConnectionId: connectionID.String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("cross-service ListConnectionResources error = %v, want NotFound", err)
	}
}

func grpcTestContext(appID uuid.UUID) context.Context {
	// Tests mirror generated SDK metadata so handler auth is exercised without
	// standing up a real gRPC listener.
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "fsk_test",
		"x-app-id", appID.String(),
	))
}

type grpcRuntimeStore struct {
	store.Store
	accountID uuid.UUID
	appID     uuid.UUID
	scope     *store.AppRuntime
}

func (s *grpcRuntimeStore) AuthorizeApp(_ context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
	if appID != s.appID || tokenHash != auth.HashToken("fsk_test") {
		return nil, errors.New("unauthorized")
	}
	return &store.AuthProjection{AccountID: s.accountID, AppFamilyID: appID, AppID: appID, Version: "1.0.0", Kind: "sdk", AppStatus: "active"}, nil
}

func (s *grpcRuntimeStore) GetAppRuntime(_ context.Context, appID uuid.UUID) (*store.AppRuntime, error) {
	if appID != s.appID || s.scope == nil {
		return nil, store.ErrAppRuntimeNotFound
	}
	return s.scope, nil
}
