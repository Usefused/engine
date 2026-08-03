package store

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

func (s *cachedStore) ResolveResourceReference(ctx context.Context, query ResourceReferenceQuery) (uuid.UUID, error) {
	resolver, ok := s.Store.(ResourceReferenceResolver)
	if !ok {
		return uuid.Nil, errors.New("store does not support resource reference resolution")
	}
	// Names and slugs can be changed by management mutations. A direct indexed
	// lookup avoids a second invalidation cache whose correctness would govern
	// which immutable UUID receives a later mutation.
	return resolver.ResolveResourceReference(ctx, query)
}

var _ ResourceReferenceResolver = (*cachedStore)(nil)
