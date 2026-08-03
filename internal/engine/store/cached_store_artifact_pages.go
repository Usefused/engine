package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *cachedStore) ListAuthorizedArtifactScopesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind string, limit, offset int) ([]ArtifactScope, int, error) {
	repository, ok := s.Store.(ArtifactPageRepository)
	if !ok {
		return nil, 0, errors.New("store does not support artifact pages")
	}
	// Permission snapshots are already cached per actor. Artifact pages remain
	// direct reads so create, rename, and deactivate operations are visible on
	// the next command without a second invalidation protocol.
	return repository.ListAuthorizedArtifactScopesByAccount(ctx, accountID, scope, kind, limit, offset)
}

var _ ArtifactPageRepository = (*cachedStore)(nil)
