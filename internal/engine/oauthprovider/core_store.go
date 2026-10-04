package oauthprovider

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/fused-open-core/oauthserver"
	"github.com/google/uuid"
)

// coreStore keeps Fused's expiry-aware persistence behind the shared protocol contract.
// PostgreSQL remains responsible for atomic consumption, quotas, and lifetime caps.
type coreStore struct {
	store.OAuthClientStore
}

var _ oauthserver.Store[uuid.UUID] = coreStore{}

// sharedClient projects protocol fields; Fused retains expiry in its own store model.
func sharedClient(client store.OAuthClient) oauthserver.OAuthClient {
	return oauthserver.OAuthClient{
		ID: client.ID, ClientID: client.ClientID, Name: client.Name, ClientType: client.ClientType,
		RedirectURIs: client.RedirectURIs, AllowedScopes: client.AllowedScopes,
		HasSecret: client.HasSecret, CreatedAt: client.CreatedAt, RevokedAt: client.RevokedAt,
	}
}

// createClient bridges permanent registration without introducing an anonymous expiry policy.
func (s coreStore) createClient(ctx context.Context, input oauthserver.OAuthClientRegistration[uuid.UUID]) (store.OAuthClientMutationResult, error) {
	return s.OAuthClientStore.CreateOAuthClient(ctx, store.OAuthClientRegistration{
		Name: input.Name, ClientType: input.ClientType, ClientID: input.ClientID,
		ClientSecretHash: input.ClientSecretHash, RedirectURIs: input.RedirectURIs,
		AllowedScopes: input.AllowedScopes, Actor: store.MutationActor(input.Actor),
	})
}

// CreateOAuthClient returns the core's projection while retaining Fused's persistence validation.
func (s coreStore) CreateOAuthClient(ctx context.Context, input oauthserver.OAuthClientRegistration[uuid.UUID]) (oauthserver.OAuthClientMutationResult, error) {
	result, err := s.createClient(ctx, input)
	return oauthserver.OAuthClientMutationResult{Client: sharedClient(result.Client), AuthorizationRevision: result.AuthorizationRevision}, err
}

// ListOAuthClients satisfies the core contract; the public Fused list retains expiry metadata.
func (s coreStore) ListOAuthClients(ctx context.Context) ([]oauthserver.OAuthClient, error) {
	clients, err := s.OAuthClientStore.ListOAuthClients(ctx)
	// Preserve storage failures instead of reporting an empty successful catalogue.
	if err != nil {
		return nil, err
	}
	result := make([]oauthserver.OAuthClient, 0, len(clients))
	// Convert only protocol fields; expiry remains available through Fused's public list.
	for _, client := range clients {
		result = append(result, sharedClient(client))
	}
	return result, nil
}

// GetOAuthClientByPublicID delegates expiry and revocation checks to Fused's durable store.
func (s coreStore) GetOAuthClientByPublicID(ctx context.Context, id string) (oauthserver.OAuthClient, string, error) {
	client, hash, err := s.OAuthClientStore.GetOAuthClientByPublicID(ctx, id)
	return sharedClient(client), hash, err
}

// RevokeOAuthClient preserves the host's audit identity during administrative revocation.
func (s coreStore) RevokeOAuthClient(ctx context.Context, id uuid.UUID, actor oauthserver.MutationActor[uuid.UUID]) (int64, error) {
	return s.OAuthClientStore.RevokeOAuthClient(ctx, id, store.MutationActor(actor))
}

// RevokeOAuthConnectedApp preserves the browser subject and audit identity for disconnection.
func (s coreStore) RevokeOAuthConnectedApp(ctx context.Context, clientID, subjectID uuid.UUID, actor oauthserver.MutationActor[uuid.UUID]) (int64, error) {
	return s.OAuthClientStore.RevokeOAuthConnectedApp(ctx, clientID, subjectID, store.MutationActor(actor))
}

// requestStore captures projections within one synchronous request. Keeping these
// values off Service prevents concurrent grants from sharing client/token metadata.
type requestStore struct {
	coreStore
	client   store.OAuthClient
	metadata store.OAuthTokenMetadata
}

// CreateOAuthClient retains the exact persisted projection for Fused's admin response.
func (s *requestStore) CreateOAuthClient(ctx context.Context, input oauthserver.OAuthClientRegistration[uuid.UUID]) (oauthserver.OAuthClientMutationResult, error) {
	result, err := s.createClient(ctx, input)
	s.client = result.Client
	return oauthserver.OAuthClientMutationResult{Client: sharedClient(result.Client), AuthorizationRevision: result.AuthorizationRevision}, err
}

// GetOAuthClientByPublicID preserves expiry metadata for the consent response.
func (s *requestStore) GetOAuthClientByPublicID(ctx context.Context, id string) (oauthserver.OAuthClient, string, error) {
	client, hash, err := s.OAuthClientStore.GetOAuthClientByPublicID(ctx, id)
	s.client = client
	return sharedClient(client), hash, err
}

// ExchangeOAuthAuthorizationCode retains the transaction's capped deadline for expires_in.
func (s *requestStore) ExchangeOAuthAuthorizationCode(ctx context.Context, input store.OAuthCodeExchange, at time.Time) (store.OAuthTokenMetadata, error) {
	metadata, err := s.OAuthClientStore.ExchangeOAuthAuthorizationCode(ctx, input, at)
	s.metadata = metadata
	return metadata, err
}

// RotateOAuthRefreshToken retains the persisted deadline instead of assuming a new full TTL.
func (s *requestStore) RotateOAuthRefreshToken(ctx context.Context, input store.OAuthRefreshExchange, at time.Time) (store.OAuthTokenMetadata, error) {
	metadata, err := s.OAuthClientStore.RotateOAuthRefreshToken(ctx, input, at)
	s.metadata = metadata
	return metadata, err
}
