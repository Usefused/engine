package store

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

func (s *cachedStore) managedIdentityStore() (ManagedIdentityStore, error) {
	repository, ok := s.Store.(ManagedIdentityStore)
	if !ok {
		return nil, errors.New("store does not support managed identity")
	}
	return repository, nil
}

func (s *cachedStore) CreateManagedLoginTransaction(ctx context.Context, transaction ManagedLoginTransaction) error {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return err
	}
	return repository.CreateManagedLoginTransaction(ctx, transaction)
}

func (s *cachedStore) ClaimManagedLoginExchange(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) (ManagedLoginTransaction, error) {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return ManagedLoginTransaction{}, err
	}
	return repository.ClaimManagedLoginExchange(ctx, id, pollSecretHash, at)
}

func (s *cachedStore) ReleaseManagedLoginExchange(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) error {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return err
	}
	return repository.ReleaseManagedLoginExchange(ctx, id, pollSecretHash, at)
}

func (s *cachedStore) RejectManagedLoginTransaction(ctx context.Context, id uuid.UUID, pollSecretHash string, at time.Time) error {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return err
	}
	return repository.RejectManagedLoginTransaction(ctx, id, pollSecretHash, at)
}

func (s *cachedStore) SaveManagedLoginAssertion(ctx context.Context, id uuid.UUID, pollSecretHash string, identity VerifiedManagedIdentity, at time.Time) error {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return err
	}
	return repository.SaveManagedLoginAssertion(ctx, id, pollSecretHash, identity, at)
}

func (s *cachedStore) ConsumeManagedLoginAssertion(ctx context.Context, id uuid.UUID, pollSecretHash string, at, sessionExpiresAt time.Time) (ManagedSessionCredential, error) {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return ManagedSessionCredential{}, err
	}
	return repository.ConsumeManagedLoginAssertion(ctx, id, pollSecretHash, at, sessionExpiresAt)
}

func (s *cachedStore) ExpireManagedLoginTransactions(ctx context.Context, at time.Time, limit int) (int64, error) {
	repository, err := s.managedIdentityStore()
	if err != nil {
		return 0, err
	}
	return repository.ExpireManagedLoginTransactions(ctx, at, limit)
}

var _ ManagedIdentityStore = (*cachedStore)(nil)
