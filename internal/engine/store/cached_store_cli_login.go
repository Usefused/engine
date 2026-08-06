package store

import (
	"context"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

func (s *cachedStore) cliLoginStore() (CLILoginStore, error) {
	repository, ok := s.Store.(CLILoginStore)
	if !ok {
		return nil, ErrCLILoginUnavailable
	}
	return repository, nil
}

func (s *cachedStore) CreateCLILoginTransaction(ctx context.Context, transaction CLILoginTransaction) error {
	repository, err := s.cliLoginStore()
	if err != nil {
		return err
	}
	return repository.CreateCLILoginTransaction(ctx, transaction)
}

func (s *cachedStore) ApproveCLILoginTransaction(ctx context.Context, id uuid.UUID, browserHash string, actor accesscontrol.Actor, at time.Time) error {
	repository, err := s.cliLoginStore()
	if err != nil {
		return err
	}
	return repository.ApproveCLILoginTransaction(ctx, id, browserHash, actor, at)
}

func (s *cachedStore) ConsumeCLILoginTransaction(ctx context.Context, id uuid.UUID, pollHash string, at time.Time) (CLILoginCredential, error) {
	repository, err := s.cliLoginStore()
	if err != nil {
		return CLILoginCredential{}, err
	}
	return repository.ConsumeCLILoginTransaction(ctx, id, pollHash, at)
}

func (s *cachedStore) RevokeCurrentCLICredential(ctx context.Context, actor MutationActor) (CLILogoutResult, error) {
	repository, err := s.cliLoginStore()
	if err != nil {
		return CLILogoutResult{}, err
	}
	return repository.RevokeCurrentCLICredential(ctx, actor)
}

var _ CLILoginStore = (*cachedStore)(nil)
