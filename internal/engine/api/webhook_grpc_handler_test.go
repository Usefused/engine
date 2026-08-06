package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
)

// webhookAuthTestStore stubs just enough of store.Store for
// authenticateWebhookSubscribe's family-scoped authorization call. Everything
// else panics via the zero-value
// embedded store.Store if a test accidentally exercises it, same pattern as
// workspaceTestStore in workspace_handlers_test.go.
type webhookAuthTestStore struct {
	store.Store
	wantAppID     uuid.UUID
	wantTokenHash string
	accountID     uuid.UUID
	err           error
}

func (s *webhookAuthTestStore) AuthorizeApp(ctx context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
	if s.err != nil {
		return nil, s.err
	}
	if appID != s.wantAppID || tokenHash != s.wantTokenHash {
		return nil, store.ErrAppNotFound
	}
	return &store.AuthProjection{AccountID: s.accountID, AppFamilyID: appID, AppID: appID, Version: "1.0.0", Kind: "sdk", AppStatus: "active"}, nil
}

// TestAuthenticateWebhookSubscribe_ReadsAppIDAndTokenFromMetadata is the
// core regression assertion for the gRPC-metadata auth migration: no
// backward compat was kept with the earlier design that read artifact_id
// and token from the first WebhookSubscribe message's fields, so this
// exercises the replacement (x-api-key/x-artifact-id metadata, mirroring
// grpcAPIKey/grpcAppID in engine_grpc.go) end to end.
func TestAuthenticateWebhookSubscribe_ReadsAppIDAndTokenFromMetadata(t *testing.T) {
	appID := uuid.New()
	wantAccountID := uuid.New()
	token := "fsk_test_token"

	s := &webhookAuthTestStore{
		wantAppID:     appID,
		wantTokenHash: auth.HashToken(token),
		accountID:     wantAccountID,
	}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", token,
		"x-app-id", appID.String(),
	))

	gotAppID, gotAccountID, err := srv.authenticateWebhookSubscribe(ctx)
	if err != nil {
		t.Fatalf("authenticateWebhookSubscribe() error = %v", err)
	}
	if gotAppID != appID {
		t.Fatalf("appID = %v, want %v", gotAppID, appID)
	}
	if gotAccountID != wantAccountID {
		t.Fatalf("accountID = %v, want %v", gotAccountID, wantAccountID)
	}
}

func TestAuthenticateWebhookSubscribe_MissingAppIDMetadataRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "fsk_test_token"))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing x-artifact-id, got %v", err)
	}
}

func TestAuthenticateWebhookSubscribe_InvalidAppIDFormatRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "fsk_test_token",
		"x-app-id", "not-a-uuid",
	))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for malformed x-artifact-id, got %v", err)
	}
}

func TestAuthenticateWebhookSubscribe_WrongTokenRejected(t *testing.T) {
	appID := uuid.New()
	s := &webhookAuthTestStore{
		wantAppID:     appID,
		wantTokenHash: auth.HashToken("the-real-token"),
		accountID:     uuid.New(),
	}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "wrong-token",
		"x-app-id", appID.String(),
	))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for a wrong token, got %v", err)
	}
}
