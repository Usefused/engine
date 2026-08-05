package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (s *cachedStore) ListArtifactServiceSummaries(ctx context.Context, artifactID uuid.UUID) ([]ArtifactServiceSummary, error) {
	repository, ok := s.Store.(ArtifactServiceRepository)
	if !ok {
		return nil, errors.New("store does not support artifact service summaries")
	}
	return repository.ListArtifactServiceSummaries(ctx, artifactID)
}

func (s *cachedStore) ListArtifactSnapshotServiceSummaries(ctx context.Context, accountID, artifactID uuid.UUID) ([]ArtifactServiceSummary, error) {
	repository, ok := s.Store.(ArtifactServiceRepository)
	if !ok {
		return nil, errors.New("store does not support artifact service summaries")
	}
	return repository.ListArtifactSnapshotServiceSummaries(ctx, accountID, artifactID)
}

var _ ArtifactServiceRepository = (*cachedStore)(nil)
