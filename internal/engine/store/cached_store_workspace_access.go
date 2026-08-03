package store

import (
	"context"
	"fmt"
)

func (s *cachedStore) workspaceAccessRepository() (WorkspaceAccessRepository, error) {
	repository, ok := s.Store.(WorkspaceAccessRepository)
	if !ok {
		return nil, fmt.Errorf("store does not support workspace access management")
	}
	return repository, nil
}

func (s *cachedStore) ListWorkspaceShares(ctx context.Context, options WorkspaceShareListOptions) ([]WorkspaceShare, int, error) {
	repository, err := s.workspaceAccessRepository()
	if err != nil {
		return nil, 0, err
	}
	return repository.ListWorkspaceShares(ctx, options)
}

func (s *cachedStore) GrantWorkspaceShare(ctx context.Context, input WorkspaceShareMutation) (WorkspaceShareMutationResult, error) {
	repository, err := s.workspaceAccessRepository()
	if err != nil {
		return WorkspaceShareMutationResult{}, err
	}
	return repository.GrantWorkspaceShare(ctx, input)
}

func (s *cachedStore) RevokeWorkspaceShare(ctx context.Context, input WorkspaceShareMutation) (WorkspaceShareMutationResult, error) {
	repository, err := s.workspaceAccessRepository()
	if err != nil {
		return WorkspaceShareMutationResult{}, err
	}
	return repository.RevokeWorkspaceShare(ctx, input)
}

var _ WorkspaceAccessRepository = (*cachedStore)(nil)
