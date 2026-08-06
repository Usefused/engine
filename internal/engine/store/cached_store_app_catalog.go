package store

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var errAppCatalogUnavailable = errors.New("app catalogue is unavailable")

func (s *cachedStore) appCatalog() (AppCatalogRepository, error) {
	repository, ok := s.Store.(AppCatalogRepository)
	if !ok {
		return nil, errAppCatalogUnavailable
	}
	return repository, nil
}

func (s *cachedStore) ListAuthorizedAppsByAccount(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind, search, version string, limit, offset int) ([]AppCatalogItem, int, error) {
	repository, err := s.appCatalog()
	if err != nil {
		return nil, 0, err
	}
	return repository.ListAuthorizedAppsByAccount(ctx, accountID, scope, kind, search, version, limit, offset)
}

func (s *cachedStore) GetAuthorizedApp(ctx context.Context, accountID, appID uuid.UUID, scope accesscontrol.AuthorizedScope) (*AppCatalogItem, error) {
	repository, err := s.appCatalog()
	if err != nil {
		return nil, err
	}
	return repository.GetAuthorizedApp(ctx, accountID, appID, scope)
}

func (s *cachedStore) ListAuthorizedAppsByFamily(ctx context.Context, accountID, appFamilyID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]AppCatalogItem, error) {
	repository, err := s.appCatalog()
	if err != nil {
		return nil, err
	}
	return repository.ListAuthorizedAppsByFamily(ctx, accountID, appFamilyID, scope)
}

func (s *cachedStore) ListAuthorizedAppServiceSummaries(ctx context.Context, accountID, appID uuid.UUID, scope accesscontrol.AuthorizedScope) ([]AppServiceSummary, error) {
	repository, err := s.appCatalog()
	if err != nil {
		return nil, err
	}
	return repository.ListAuthorizedAppServiceSummaries(ctx, accountID, appID, scope)
}

var _ AppCatalogRepository = (*cachedStore)(nil)
