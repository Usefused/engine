package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

func (s *cachedStore) userRepository() (UserRepository, error) {
	repository, ok := s.Store.(UserRepository)
	if !ok {
		return nil, fmt.Errorf("store does not support user management")
	}
	return repository, nil
}

func (s *cachedStore) CreateUser(ctx context.Context, input CreateUserInput) (UserMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return UserMutationResult{}, err
	}
	return repository.CreateUser(ctx, input)
}

func (s *cachedStore) GetUser(ctx context.Context, id uuid.UUID) (User, error) {
	repository, err := s.userRepository()
	if err != nil {
		return User{}, err
	}
	return repository.GetUser(ctx, id)
}

func (s *cachedStore) ListUsers(ctx context.Context, options UserListOptions) ([]User, int, error) {
	repository, err := s.userRepository()
	if err != nil {
		return nil, 0, err
	}
	return repository.ListUsers(ctx, options)
}

func (s *cachedStore) UpdateUser(ctx context.Context, id uuid.UUID, patch UserPatch) (UserMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return UserMutationResult{}, err
	}
	return repository.UpdateUser(ctx, id, patch)
}

func (s *cachedStore) SuspendUser(ctx context.Context, id uuid.UUID, actor MutationActor) (UserMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return UserMutationResult{}, err
	}
	return repository.SuspendUser(ctx, id, actor)
}

func (s *cachedStore) ReactivateUser(ctx context.Context, id uuid.UUID, actor MutationActor) (UserMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return UserMutationResult{}, err
	}
	return repository.ReactivateUser(ctx, id, actor)
}

func (s *cachedStore) AddTeamMember(ctx context.Context, input TeamMemberMutation) (MembershipMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return MembershipMutationResult{}, err
	}
	return repository.AddTeamMember(ctx, input)
}

func (s *cachedStore) AddTeamMemberByEmail(ctx context.Context, input AddTeamMemberByEmailInput) (MembershipMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return MembershipMutationResult{}, err
	}
	return repository.AddTeamMemberByEmail(ctx, input)
}

func (s *cachedStore) RemoveTeamMember(ctx context.Context, teamID, userID uuid.UUID, actor MutationActor) (MembershipMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return MembershipMutationResult{}, err
	}
	return repository.RemoveTeamMember(ctx, teamID, userID, actor)
}

func (s *cachedStore) ListTeamMembers(ctx context.Context, teamID uuid.UUID, options UserListOptions) ([]TeamMember, int, error) {
	repository, err := s.userRepository()
	if err != nil {
		return nil, 0, err
	}
	return repository.ListTeamMembers(ctx, teamID, options)
}

func (s *cachedStore) GetUserEffectiveAccess(ctx context.Context, userID uuid.UUID) ([]EffectiveAccessGrant, int64, error) {
	repository, err := s.userRepository()
	if err != nil {
		return nil, 0, err
	}
	return repository.GetUserEffectiveAccess(ctx, userID)
}

func (s *cachedStore) IssueUserControlCredential(ctx context.Context, input IssueCredentialInput) (IssuedControlCredential, error) {
	repository, err := s.userRepository()
	if err != nil {
		return IssuedControlCredential{}, err
	}
	return repository.IssueUserControlCredential(ctx, input)
}

func (s *cachedStore) ListUserControlCredentials(ctx context.Context, userID uuid.UUID) ([]ControlCredential, error) {
	repository, err := s.userRepository()
	if err != nil {
		return nil, err
	}
	return repository.ListUserControlCredentials(ctx, userID)
}

func (s *cachedStore) RevokeUserControlCredential(ctx context.Context, userID, credentialID uuid.UUID, actor MutationActor) (CredentialMutationResult, error) {
	repository, err := s.userRepository()
	if err != nil {
		return CredentialMutationResult{}, err
	}
	return repository.RevokeUserControlCredential(ctx, userID, credentialID, actor)
}

var _ UserRepository = (*cachedStore)(nil)
