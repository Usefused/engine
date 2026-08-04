package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *cachedStore) ListServiceConsumers(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, serviceID uuid.UUID) ([]ServiceConsumer, error) {
	repository, ok := s.Store.(ServiceConsumerRepository)
	if !ok {
		return nil, errors.New("store does not support service consumer lookup")
	}
	return repository.ListServiceConsumers(ctx, accountID, scope, serviceID)
}

var _ ServiceConsumerRepository = (*cachedStore)(nil)
