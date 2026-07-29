package api

import (
	"context"
	"strings"
	"testing"
	"time"

	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

// TestEngineGRPCStartConnectSessionCreatesAuthorizationURL covers the
// generated-SDK path, proving it shares the same Engine-owned session creation
// as REST/GraphQL instead of minting provider URLs client-side.
func TestEngineGRPCStartConnectSessionCreatesAuthorizationURL(t *testing.T) {
	fixture := newConnectRuntimeFixture(t)
	srv := NewEngineGRPCServer(fixture.store, fixture.verifier, fixture.masterKey)
	ctx := grpcTestContext()
	artifactID := attachConnectTestArtifact(&fixture)

	resp, err := srv.StartConnectSession(ctx, &enginev1.StartConnectSessionRequest{
		BucketId:            fixture.bucketID.String(),
		ServiceId:           fixture.serviceID.String(),
		EndUserRef:          "user_123",
		ReturnUrl:           "https://app.example.com/oauth/done",
		CreatedByArtifactId: artifactID.String(),
		Scopes:              []string{"openid"},
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
	srv := NewEngineGRPCServer(s, &mockVerifier{}, []byte("12345678901234567890123456789012"))

	resp, err := srv.GetConnection(grpcTestContext(), &enginev1.GetConnectionRequest{ConnectionId: connectionID.String()})
	if err != nil {
		t.Fatalf("GetConnection() error = %v", err)
	}
	if !resp.GetFound() || resp.GetConnection().GetEndUserRef() != "user_123" {
		t.Fatalf("unexpected connection response: %#v", resp)
	}
	if strings.Contains(resp.String(), "encrypted-access-token") {
		t.Fatalf("connection response leaked encrypted token material: %s", resp.String())
	}
}

func grpcTestContext() context.Context {
	// Tests mirror generated SDK metadata so handler auth is exercised without
	// standing up a real gRPC listener.
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "fsk_test"))
}
