package sandbox

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

type ActivationManager struct {
	client RegistryClient
	store  store.Store
}

func NewActivationManager(client RegistryClient, s store.Store) *ActivationManager {
	return &ActivationManager{
		client: client,
		store:  s,
	}
}

func (m *ActivationManager) ActivateService(ctx context.Context, serviceID string, version string) error {
	svcUUID, err := uuid.Parse(serviceID)
	if err != nil {
		return fmt.Errorf("invalid service ID format: %w", err)
	}

	// 1. Fetch from registry to validate it exists
	meta, err := m.client.FetchServiceMetadata(ctx, svcUUID, version)
	if err != nil {
		return fmt.Errorf("failed to fetch service metadata from registry: %w", err)
	}

	// 2. Record Activation
	// The bucket UI resolves linked-service labels from fused_workspace_services.
	// Persist the registry name we just fetched so runtime-triggered activation
	// cannot leave later bucket reads showing raw service UUIDs.
	if err := m.store.AddWorkspaceServiceVersion(ctx, svcUUID, "", version, meta.ServiceVersionID, meta.Name, uuid.Nil); err != nil {
		return fmt.Errorf("failed to record activation: %w", err)
	}

	slog.InfoContext(ctx, "Service successfully activated", slog.String("serviceID", serviceID))
	return nil
}

func (m *ActivationManager) RefreshService(ctx context.Context, serviceID string, version string) error {
	svcUUID, err := uuid.Parse(serviceID)
	if err != nil {
		return fmt.Errorf("invalid service ID format: %w", err)
	}

	// 1. Fetch from registry to ensure it's still valid
	_, err = m.client.FetchServiceMetadata(ctx, svcUUID, version)
	if err != nil {
		return fmt.Errorf("failed to fetch service metadata from registry: %w", err)
	}

	// Invalidation is handled by the cache itself (if we had access to it here),
	// or we just rely on TTL/reconnects.

	slog.InfoContext(ctx, "Service definition successfully refreshed", slog.String("serviceID", serviceID))
	return nil
}
