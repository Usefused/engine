package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

func (s *cachedStore) oauthClientStore() (OAuthClientStore, error) {
	repository, ok := s.Store.(OAuthClientStore)
	if !ok {
		return nil, fmt.Errorf("store does not support OAuth provider management")
	}
	return repository, nil
}

func (s *cachedStore) CreateOAuthClient(ctx context.Context, input OAuthClientRegistration) (OAuthClientMutationResult, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	return repository.CreateOAuthClient(ctx, input)
}

func (s *cachedStore) RegisterOAuthClient(ctx context.Context, input OAuthClientRegistration) (OAuthClientMutationResult, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthClientMutationResult{}, err
	}
	return repository.RegisterOAuthClient(ctx, input)
}

func (s *cachedStore) ListOAuthClients(ctx context.Context) ([]OAuthClient, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return nil, err
	}
	return repository.ListOAuthClients(ctx)
}

func (s *cachedStore) RevokeOAuthClient(ctx context.Context, id uuid.UUID, actor MutationActor) (int64, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return 0, err
	}
	return repository.RevokeOAuthClient(ctx, id, actor)
}

func (s *cachedStore) GetOAuthClientByPublicID(ctx context.Context, clientID string) (OAuthClient, string, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthClient{}, "", err
	}
	return repository.GetOAuthClientByPublicID(ctx, clientID)
}

func (s *cachedStore) GetOAuthUserConsent(ctx context.Context, clientID, subjectID uuid.UUID) (OAuthConsent, bool, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthConsent{}, false, err
	}
	return repository.GetOAuthUserConsent(ctx, clientID, subjectID)
}

func (s *cachedStore) RecordOAuthConsentAndIssueCode(ctx context.Context, consent OAuthConsentGrant, code OAuthAuthorizationCodeIssue) error {
	repository, err := s.oauthClientStore()
	if err != nil {
		return err
	}
	return repository.RecordOAuthConsentAndIssueCode(ctx, consent, code)
}

func (s *cachedStore) ExchangeOAuthAuthorizationCode(ctx context.Context, input OAuthCodeExchange, at time.Time) (OAuthTokenMetadata, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	return repository.ExchangeOAuthAuthorizationCode(ctx, input, at)
}

func (s *cachedStore) RotateOAuthRefreshToken(ctx context.Context, input OAuthRefreshExchange, at time.Time) (OAuthTokenMetadata, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthTokenMetadata{}, err
	}
	return repository.RotateOAuthRefreshToken(ctx, input, at)
}

func (s *cachedStore) RevokeOAuthTokenByHash(ctx context.Context, clientID uuid.UUID, tokenHash string) (bool, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return false, err
	}
	return repository.RevokeOAuthTokenByHash(ctx, clientID, tokenHash)
}

func (s *cachedStore) ListOAuthConnectedApps(ctx context.Context, subjectID uuid.UUID) ([]OAuthConnectedApp, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return nil, err
	}
	return repository.ListOAuthConnectedApps(ctx, subjectID)
}

func (s *cachedStore) RevokeOAuthConnectedApp(ctx context.Context, clientID, subjectID uuid.UUID, actor MutationActor) (int64, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return 0, err
	}
	return repository.RevokeOAuthConnectedApp(ctx, clientID, subjectID, actor)
}

func (s *cachedStore) ExpireOAuthArtifacts(ctx context.Context, at time.Time, limit int) (int, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return 0, err
	}
	return repository.ExpireOAuthArtifacts(ctx, at, limit)
}

func (s *cachedStore) SetOAuthRegistrationKey(ctx context.Context, subjectID uuid.UUID, keyHash string, actor MutationActor) (OAuthRegistrationKey, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return OAuthRegistrationKey{}, err
	}
	return repository.SetOAuthRegistrationKey(ctx, subjectID, keyHash, actor)
}

func (s *cachedStore) RevokeOAuthRegistrationKey(ctx context.Context, subjectID uuid.UUID, actor MutationActor) error {
	repository, err := s.oauthClientStore()
	if err != nil {
		return err
	}
	return repository.RevokeOAuthRegistrationKey(ctx, subjectID, actor)
}

func (s *cachedStore) GetOAuthRegistrationKeySubject(ctx context.Context, keyHash string) (uuid.UUID, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return uuid.Nil, err
	}
	return repository.GetOAuthRegistrationKeySubject(ctx, keyHash)
}

func (s *cachedStore) HasOAuthRegistrationKey(ctx context.Context, subjectID uuid.UUID) (bool, error) {
	repository, err := s.oauthClientStore()
	if err != nil {
		return false, err
	}
	return repository.HasOAuthRegistrationKey(ctx, subjectID)
}

var _ OAuthClientStore = (*cachedStore)(nil)
