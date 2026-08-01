package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

func (s *cachedStore) teamRepository() (TeamRepository, error) {
	repository, ok := s.Store.(TeamRepository)
	if !ok {
		return nil, fmt.Errorf("store does not support team management")
	}
	return repository, nil
}

func (s *cachedStore) CreateTeam(ctx context.Context, input TeamMutation) (TeamMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamMutationResult{}, err
	}
	return repository.CreateTeam(ctx, input)
}

func (s *cachedStore) GetTeam(ctx context.Context, teamID uuid.UUID) (Team, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return Team{}, err
	}
	return repository.GetTeam(ctx, teamID)
}

func (s *cachedStore) ListTeams(ctx context.Context, options TeamListOptions) ([]Team, int, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return nil, 0, err
	}
	return repository.ListTeams(ctx, options)
}

func (s *cachedStore) UpdateTeam(ctx context.Context, teamID uuid.UUID, patch TeamPatch) (TeamMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamMutationResult{}, err
	}
	return repository.UpdateTeam(ctx, teamID, patch)
}

func (s *cachedStore) ArchiveTeam(ctx context.Context, teamID uuid.UUID, actor MutationActor) (TeamMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamMutationResult{}, err
	}
	return repository.ArchiveTeam(ctx, teamID, actor)
}

func (s *cachedStore) AddTeamBinding(ctx context.Context, input TeamBindingMutation) (TeamBindingMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamBindingMutationResult{}, err
	}
	return repository.AddTeamBinding(ctx, input)
}

func (s *cachedStore) RemoveTeamBinding(ctx context.Context, input TeamBindingMutation) (TeamBindingMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamBindingMutationResult{}, err
	}
	return repository.RemoveTeamBinding(ctx, input)
}

func (s *cachedStore) ClearTeamWorkspaceRole(ctx context.Context, teamID, workspaceID uuid.UUID, actor MutationActor) (TeamBindingMutationResult, error) {
	repository, err := s.teamRepository()
	if err != nil {
		return TeamBindingMutationResult{}, err
	}
	return repository.ClearTeamWorkspaceRole(ctx, teamID, workspaceID, actor)
}

var _ TeamRepository = (*cachedStore)(nil)
