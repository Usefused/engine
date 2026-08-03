package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (s *cachedStore) artifactAccessRepository() (ArtifactAccessRepository, error) {
	repository, ok := s.Store.(ArtifactAccessRepository)
	if !ok {
		return nil, errors.New("artifact access repository is unavailable")
	}
	return repository, nil
}

func (s *cachedStore) PreflightArtifactOwnership(ctx context.Context, input ArtifactOwnershipPreflight) (ArtifactOwnershipDecision, error) {
	repository, err := s.artifactAccessRepository()
	if err != nil {
		return ArtifactOwnershipDecision{}, err
	}
	return repository.PreflightArtifactOwnership(ctx, input)
}

func (s *cachedStore) ListArtifactBuildSelectors(ctx context.Context, input ArtifactSelectorQuery) (ArtifactSelectorPage, error) {
	repository, err := s.artifactAccessRepository()
	if err != nil {
		return ArtifactSelectorPage{}, err
	}
	return repository.ListArtifactBuildSelectors(ctx, input)
}

func (s *cachedStore) ListArtifactOwningTeams(ctx context.Context, input ActorTeamSelectorQuery) (ArtifactOwningTeamPage, error) {
	repository, err := s.artifactAccessRepository()
	if err != nil {
		return ArtifactOwningTeamPage{}, err
	}
	return repository.ListArtifactOwningTeams(ctx, input)
}

func (s *cachedStore) ResolveArtifactOwningTeamReference(ctx context.Context, input ArtifactOwningTeamReferenceQuery) (uuid.UUID, error) {
	repository, err := s.artifactAccessRepository()
	if err != nil {
		return uuid.Nil, err
	}
	return repository.ResolveArtifactOwningTeamReference(ctx, input)
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
	_ ArtifactAccessRepository   = (*cachedStore)(nil)
	_ AccessInspectionRepository = (*cachedStore)(nil)
	_ AuditRepository            = (*cachedStore)(nil)
)
