package store

import (
	"context"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// generationContracts delegates every planning read freshly so refresh cannot be hidden by a process cache.
func (s *cachedStore) generationContracts() (GenerationContractStore, error) {
	delegate, ok := s.Store.(GenerationContractStore)
	// Missing persistence support cannot establish generation authority.
	if !ok {
		return nil, ErrGenerationContractPinUnavailable
	}
	return delegate, nil
}

// ResolveGenerationServiceIDsByKeys preserves provider qualification through the canonical SQL resolver without caching visibility.
func (s *cachedStore) ResolveGenerationServiceIDsByKeys(ctx context.Context, keys []string) (map[string]uuid.UUID, error) {
	delegate, err := s.generationContracts()
	// Missing local authority cannot be repaired through an unscoped live lookup.
	if err != nil {
		return nil, err
	}
	return delegate.ResolveGenerationServiceIDsByKeys(ctx, keys)
}

// ListGenerationContractBindings preserves exact local revision checks across cached store wrappers.
func (s *cachedStore) ListGenerationContractBindings(ctx context.Context, refs []models.ServiceVersionRef, requireGenerationPin bool) ([]models.SDKContractBinding, error) {
	delegate, err := s.generationContracts()
	// Fail closed instead of querying a live catalogue when the store lacks pins.
	if err != nil {
		return nil, err
	}
	return delegate.ListGenerationContractBindings(ctx, refs, requireGenerationPin)
}

// ListGenerationAuthContracts forwards the bounded minimal-security projection without caching snapshots.
func (s *cachedStore) ListGenerationAuthContracts(ctx context.Context, selections []GenerationAuthSelection, requireGenerationPin bool) ([]GenerationAuthContract, error) {
	delegate, err := s.generationContracts()
	// Missing local contracts must never be interpreted as anonymous operations.
	if err != nil {
		return nil, err
	}
	return delegate.ListGenerationAuthContracts(ctx, selections, requireGenerationPin)
}

// ValidateGenerationSelections retains the SQL-owned operation and webhook membership boundary.
func (s *cachedStore) ValidateGenerationSelections(ctx context.Context, selections []models.SDKSelection, requireGenerationPin bool) error {
	delegate, err := s.generationContracts()
	// A wrapper cannot weaken the authoritative store requirement.
	if err != nil {
		return err
	}
	return delegate.ValidateGenerationSelections(ctx, selections, requireGenerationPin)
}

// ResolveGenerationSelections preserves concrete local IDs across the cache wrapper without caching catalogue scope.
func (s *cachedStore) ResolveGenerationSelections(ctx context.Context, selections []models.SDKSelection) ([]models.SDKSelection, error) {
	resolver, ok := s.Store.(GenerationSelectionResolver)
	// A wrapper without the exact local projection cannot safely publish a direct API runtime.
	if !ok {
		return nil, ErrGenerationContractPinUnavailable
	}
	return resolver.ResolveGenerationSelections(ctx, selections)
}
