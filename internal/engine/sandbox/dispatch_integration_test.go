package sandbox

import (
	"context"
	"fmt"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type dummyTokenValidator struct{}

func (v *dummyTokenValidator) Validate(ctx context.Context, artifactID uuid.UUID, token string) (uuid.UUID, error) {
	return uuid.New(), nil
}

// richMockCache serves a full scope + Fused object so engineExecuteCore can be
// exercised end-to-end against a mock vendor.
type richMockCache struct {
	scopeJSON []byte
	obj       *fusedobject.ServiceMetadata
	epID      uuid.UUID
	// path/method let dispatch-chain tests exercise path-parameter binding
	// materialization (e.g. "/items/{accountId}"); the zero value preserves
	// the pathless behavior every other caller of this fixture already relies on.
	path   string
	method string
}

func (m *richMockCache) ConnectSDK(ctx context.Context, artifactID string) error { return nil }
func (m *richMockCache) DisconnectSDK(artifactID string)                         {}

func (m *richMockCache) GetOrFetchServiceMetadata(ctx context.Context, artifactID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	return m.obj, nil
}
func (c *richMockCache) GetEndpoint(ctx context.Context, artifactID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	if endpointName == "list_items" || endpointName == "do_thing" {
		return &fusedobject.Endpoint{Name: endpointName, ID: c.epID, Path: c.path, Method: c.method}, nil
	}
	return nil, fmt.Errorf("not found")
}
func (m *richMockCache) GetArtifactScope(ctx context.Context, artifactID string) (string, []byte, error) {
	return "test", m.scopeJSON, nil
}
func (m *richMockCache) Invalidate(serviceID string)               {}
func (m *richMockCache) InvalidateArtifactScope(artifactID string) {}

// ListEndpointsForSelection mirrors GetEndpoint's permissive stub behavior:
// this mock backs engineExecuteCore's dispatch path, not fixture-building
// tests, so it just hands back the same fixed "list_items"/"do_thing" pair
// GetEndpoint already recognizes.
func (m *richMockCache) ListEndpointsForSelection(ctx context.Context, artifactID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	return []fusedobject.Endpoint{
		{Name: "list_items", ID: m.epID},
		{Name: "do_thing", ID: m.epID},
	}, nil
}
