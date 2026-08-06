package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *cachedStore) ListAuthorizedAppRuntimesByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind string, limit, offset int) ([]AppRuntime, int, error) {
	repository, ok := s.Store.(AppRuntimePageRepository)
	if !ok {
		return nil, 0, errors.New("store does not support app runtime pages")
	}
	// Permission snapshots are already cached per actor. App pages remain
	// direct reads so create, rename, and deactivate operations are visible on
	// the next command without a second invalidation protocol.
	return repository.ListAuthorizedAppRuntimesByAccount(ctx, accountID, scope, kind, limit, offset)
}

var _ AppRuntimePageRepository = (*cachedStore)(nil)
