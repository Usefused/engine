package api

import (
	"context"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/apptokeninvalidation"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/webhookstream"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
)

// webhookEventLoopTestStream supplies only the context used by the cancellation-aware loop.
type webhookEventLoopTestStream struct {
	enginev1.EngineService_SubscribeWebhooksServer
	ctx context.Context
}

// Context returns the test-controlled lifetime without starting a network gRPC server.
func (stream *webhookEventLoopTestStream) Context() context.Context {
	return stream.ctx
}

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

// AuthorizeApp returns a complete runtime identity after verifying the metadata-derived lookup inputs.
func (s *webhookAuthTestStore) AuthorizeApp(ctx context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
	// Configured failures exercise authentication denial without leaking the underlying store error.
	if s.err != nil {
		return nil, s.err
	}
	// Both exact app and token digest must match the values expected from gRPC metadata.
	if appID != s.wantAppID || tokenHash != s.wantTokenHash {
		return nil, store.ErrAppNotFound
	}
	return &store.AuthProjection{
		AccountID: s.accountID, AppFamilyID: appID, AppID: appID, TokenID: uuid.New(), Version: "1.0.0",
		Kind: store.AppKindSDK, AppStatus: store.AppStatusActive,
	}, nil
}

func newWebhookAuthTestServer(s *webhookAuthTestStore) *EngineGRPCServer {
	return NewEngineGRPCServer(s, nil, nil, nil, nil, auth.NewTokenValidator(s))
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
	srv := newWebhookAuthTestServer(s)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", token,
		"x-app-id", appID.String(),
	))

	gotAppID, gotAccountID, gotFamilyID, err := srv.authenticateWebhookSubscribe(ctx)
	if err != nil {
		t.Fatalf("authenticateWebhookSubscribe() error = %v", err)
	}
	if gotAppID != appID {
		t.Fatalf("appID = %v, want %v", gotAppID, appID)
	}
	if gotAccountID != wantAccountID {
		t.Fatalf("accountID = %v, want %v", gotAccountID, wantAccountID)
	}
	if gotFamilyID != appID {
		t.Fatalf("appFamilyID = %v, want %v", gotFamilyID, appID)
	}
}

// TestAuthenticateWebhookSubscribe_MissingAppIDMetadataRejected prevents workspace-wide fallback without exact app identity.
func TestAuthenticateWebhookSubscribe_MissingAppIDMetadataRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := newWebhookAuthTestServer(s)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "fsk_test_token"))

	_, _, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for missing x-artifact-id, got %v", err)
	}
}

// TestAuthenticateWebhookSubscribe_InvalidAppIDFormatRejected rejects malformed durable and authorization identity.
func TestAuthenticateWebhookSubscribe_InvalidAppIDFormatRejected(t *testing.T) {
	s := &webhookAuthTestStore{}
	srv := newWebhookAuthTestServer(s)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "fsk_test_token",
		"x-app-id", "not-a-uuid",
	))

	_, _, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument for malformed x-artifact-id, got %v", err)
	}
}

// TestAuthenticateWebhookSubscribe_WrongTokenRejected keeps family authorization bound to the presented bearer digest.
func TestAuthenticateWebhookSubscribe_WrongTokenRejected(t *testing.T) {
	appID := uuid.New()
	s := &webhookAuthTestStore{
		wantAppID:     appID,
		wantTokenHash: auth.HashToken("the-real-token"),
		accountID:     uuid.New(),
	}
	srv := newWebhookAuthTestServer(s)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
		"x-api-key", "wrong-token",
		"x-app-id", appID.String(),
	))

	_, _, _, err := srv.authenticateWebhookSubscribe(ctx)
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated for a wrong token, got %v", err)
	}
}

// TestWebhookDurableNameIsolatesSiblingVersions prevents one family's concurrent SDK versions from overwriting each other's immutable filters.
func TestWebhookDurableNameIsolatesSiblingVersions(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()
	firstAppID := uuid.New()
	secondAppID := uuid.New()
	first := webhookDurableName(accountID, familyID, firstAppID, "auth-worker")
	second := webhookDurableName(accountID, familyID, secondAppID, "auth-worker")
	// Stable receiver names share pending work only within one exact immutable SDK version.
	if first == second {
		t.Fatalf("sibling versions shared durable name %q", first)
	}
	if first != webhookDurableName(accountID, familyID, firstAppID, "auth-worker") {
		t.Fatal("same exact app and receiver did not retain a stable durable name")
	}
}

// TestWebhookDeliveryExhaustionAllowsThreeHandlerAttempts locks the broker-only terminal interception boundary.
func TestWebhookDeliveryExhaustionAllowsThreeHandlerAttempts(t *testing.T) {
	for delivered := uint64(1); delivered <= webhookMaxHandlerAttempts; delivered++ {
		// Every configured public attempt must reach the generated handler before exhaustion is declared.
		if webhookDeliveryExhausted(delivered) {
			t.Fatalf("delivery %d exhausted before handler attempt %d", delivered, webhookMaxHandlerAttempts)
		}
	}
	if !webhookDeliveryExhausted(webhookBrokerMaxDeliver) {
		t.Fatalf("broker terminal delivery %d was not intercepted", webhookBrokerMaxDeliver)
	}
}

// TestWebhookStreamInvalidationStopsActiveReceiver verifies terminal token revocation and retriable runtime refresh statuses.
func TestWebhookStreamInvalidationStopsActiveReceiver(t *testing.T) {
	tests := []struct {
		name     string
		wantCode codes.Code
		cancel   func(*webhookstream.Registry, uuid.UUID, uuid.UUID)
	}{
		{
			name: "revoked token is terminal", wantCode: codes.PermissionDenied,
			cancel: func(registry *webhookstream.Registry, tokenID, _ uuid.UUID) {
				// Production fanout reaches the validator first and the live registry in the same synchronous boundary.
				apptokeninvalidation.NewFanoutInvalidator(registry).InvalidateToken(tokenID)
			},
		},
		{
			name: "runtime change is retriable", wantCode: codes.Unavailable,
			cancel: func(registry *webhookstream.Registry, _, appID uuid.UUID) {
				// Exact-app invalidation may represent a safe immutable refresh or a deactivation decided on reconnect.
				registry.InvalidateAppRuntime(appID)
			},
		},
	}
	// Each cancellation class must independently terminate an otherwise blocked client receiver.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := webhookstream.NewRegistry()
			tokenID, appID := uuid.New(), uuid.New()
			registration, ok := registry.Register(tokenID, appID)
			// This unit fixture represents completed source revalidation before the active event loop begins.
			if !ok || !registration.Confirm() {
				t.Fatal("failed to confirm webhook registration fixture")
			}
			server := &EngineGRPCServer{}
			result := make(chan error, 1)
			stream := &webhookEventLoopTestStream{ctx: context.Background()}
			// The loop blocks on client traffic until the local invalidation signal wins.
			go func() {
				result <- server.processGRPCWebhookEvents(
					stream, newPendingWebhookMsgs(), webhookSubscriptionScope{appID: appID, tokenID: tokenID}, registration,
					make(chan webhookClientReceiveResult), make(chan time.Time),
				)
			}()

			test.cancel(registry, tokenID, appID)
			select {
			case err := <-result:
				// The transport status tells generated receivers whether the same runtime may be retried.
				if status.Code(err) != test.wantCode {
					t.Fatalf("cancellation code = %s, want %s: %v", status.Code(err), test.wantCode, err)
				}
			case <-time.After(time.Second):
				// Synchronous cancellation must not leave a receiver able to accept another NATS delivery.
				t.Fatal("webhook event loop did not stop after invalidation")
			}
		})
	}
}
