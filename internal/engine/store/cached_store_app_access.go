package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (s *cachedStore) appAccessRepository() (AppAccessRepository, error) {
	repository, ok := s.Store.(AppAccessRepository)
	if !ok {
		return nil, errors.New("app access repository is unavailable")
	}
	return repository, nil
}

func (s *cachedStore) PreflightAppOwnership(ctx context.Context, input AppOwnershipPreflight) (AppOwnershipDecision, error) {
	repository, err := s.appAccessRepository()
	if err != nil {
		return AppOwnershipDecision{}, err
	}
	return repository.PreflightAppOwnership(ctx, input)
}

func (s *cachedStore) ResolveAppFamilyAccess(ctx context.Context, accountID uuid.UUID, appIDs []uuid.UUID) (map[uuid.UUID]uuid.UUID, error) {
	resolver, ok := s.Store.(AppFamilyAccessResolver)
	if !ok {
		return nil, errors.New("app family access resolver is unavailable")
	}
	return resolver.ResolveAppFamilyAccess(ctx, accountID, appIDs)
}

func (s *cachedStore) ListAppBuildSelectors(ctx context.Context, input AppSelectorQuery) (AppSelectorPage, error) {
	repository, err := s.appAccessRepository()
	if err != nil {
		return AppSelectorPage{}, err
	}
	return repository.ListAppBuildSelectors(ctx, input)
}

func (s *cachedStore) ListAppOwningTeams(ctx context.Context, input ActorTeamSelectorQuery) (AppOwningTeamPage, error) {
	repository, err := s.appAccessRepository()
	if err != nil {
		return AppOwningTeamPage{}, err
	}
	return repository.ListAppOwningTeams(ctx, input)
}

func (s *cachedStore) ResolveAppOwningTeamReference(ctx context.Context, input AppOwningTeamReferenceQuery) (uuid.UUID, error) {
	repository, err := s.appAccessRepository()
	if err != nil {
		return uuid.Nil, err
	}
	return repository.ResolveAppOwningTeamReference(ctx, input)
}

func (s *cachedStore) ExplainAccess(ctx context.Context, input AccessExplanationQuery) (AccessExplanation, error) {
	repository, ok := s.Store.(AccessInspectionRepository)
	if !ok {
		return AccessExplanation{}, errors.New("access inspection repository is unavailable")
	}
	return repository.ExplainAccess(ctx, input)
}

func (s *cachedStore) QueryAuditEvents(ctx context.Context, input AuditQuery) (AuditPage, error) {
	repository, ok := s.Store.(AuditRepository)
	if !ok {
		return AuditPage{}, errors.New("audit repository is unavailable")
	}
	return repository.QueryAuditEvents(ctx, input)
}

func (s *cachedStore) ExportAuditEvents(ctx context.Context, input AuditExportQuery) ([]AuditExportRow, error) {
	repository, ok := s.Store.(AuditRepository)
	if !ok {
		return nil, errors.New("audit repository is unavailable")
	}
	return repository.ExportAuditEvents(ctx, input)
}

var (
	_ AppAccessRepository        = (*cachedStore)(nil)
	_ AppFamilyAccessResolver    = (*cachedStore)(nil)
	_ AccessInspectionRepository = (*cachedStore)(nil)
	_ AuditRepository            = (*cachedStore)(nil)
)
