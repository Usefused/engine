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
// authenticateWebhookSubscribe's auth.TokenValidator.Validate call
// (store.ValidateToken) -- everything else panics via the zero-value
// embedded store.Store if a test accidentally exercises it, same pattern as
// workspaceTestStore in workspace_handlers_test.go.
type webhookAuthTestStore struct {
	store.Store
	wantArtifactID uuid.UUID
	wantTokenHash  string
	accountID      uuid.UUID
	err            error
}

func (s *webhookAuthTestStore) ValidateToken(ctx context.Context, artifactID uuid.UUID, tokenHash string) (uuid.UUID, error) {
	if s.err != nil {
		return uuid.Nil, s.err
	}
	if artifactID != s.wantArtifactID || tokenHash != s.wantTokenHash {
		return uuid.Nil, store.ErrArtifactScopeNotFound
	}
	return s.accountID, nil
}

// TestAuthenticateWebhookSubscribe_ReadsArtifactIDAndTokenFromMetadata is the
// core regression assertion for the gRPC-metadata auth migration: no
// backward compat was kept with the earlier design that read artifact_id
// and token from the first WebhookSubscribe message's fields, so this
// exercises the replacement (x-api-key/x-artifact-id metadata, mirroring
// grpcAPIKey/grpcArtifactID in engine_grpc.go) end to end.
func TestAuthenticateWebhookSubscribe_ReadsArtifactIDAndTokenFromMetadata(t *testing.T) {
	artifactID := uuid.New()
	wantAccountID := uuid.New()
	token := "fsk_test_token"

	s := &webhookAuthTestStore{
		wantArtifactID: artifactID,
		wantTokenHash:  auth.HashToken(token),
		accountID:      wantAccountID,
	}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", token,
		"x-artifact-id", artifactID.String(),
	))

	gotArtifactID, gotAccountID, err := srv.authenticateWebhookSubscribe(ctx)
	if err != nil {
		t.Fatalf("authenticateWebhookSubscribe() error = %v", err)
	}
	if gotArtifactID != artifactID {
		t.Fatalf("artifactID = %v, want %v", gotArtifactID, artifactID)
	}
	if gotAccountID != wantAccountID {
		t.Fatalf("accountID = %v, want %v", gotAccountID, wantAccountID)
	}
}

func TestAuthenticateWebhookSubscribe_MissingArtifactIDMetadataRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "fsk_test_token"))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing x-artifact-id, got %v", err)
	}
}

func TestAuthenticateWebhookSubscribe_InvalidArtifactIDFormatRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "fsk_test_token",
		"x-artifact-id", "not-a-uuid",
	))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for malformed x-artifact-id, got %v", err)
	}
}

func TestAuthenticateWebhookSubscribe_WrongTokenRejected(t *testing.T) {
	artifactID := uuid.New()
	s := &webhookAuthTestStore{
		wantArtifactID: artifactID,
		wantTokenHash:  auth.HashToken("the-real-token"),
		accountID:      uuid.New(),
	}
	srv := NewEngineGRPCServer(s, nil, nil, nil, nil)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "wrong-token",
		"x-artifact-id", artifactID.String(),
	))

	_, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for a wrong token, got %v", err)
	}
}
