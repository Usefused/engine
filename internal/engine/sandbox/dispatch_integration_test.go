package sandbox

import (
	"context"
	"fmt"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type dummyTokenValidator struct{}

func (v *dummyTokenValidator) Validate(ctx context.Context, appID uuid.UUID, token string) (auth.RuntimeIdentity, error) {
	return auth.RuntimeIdentity{AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: appID, AppVersion: "1.0.0", Kind: "sdk", Status: "active", TokenPolicy: store.AppTokenPolicy{AllowAll: true}}, nil
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
	path                 string
	method               string
	securityRequirements authrouting.Requirements
}

func (m *richMockCache) ConnectSDK(ctx context.Context, appID string) error { return nil }
func (m *richMockCache) DisconnectSDK(appID string)                         {}

func (m *richMockCache) GetOrFetchServiceMetadata(ctx context.Context, appID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	return m.obj, nil
}
func (c *richMockCache) GetEndpoint(ctx context.Context, appID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	if endpointName == "list_items" || endpointName == "do_thing" {
		return &fusedobject.Endpoint{Name: endpointName, ID: c.epID, Path: c.path, Method: c.method, SecurityRequirements: c.testSecurityRequirements()}, nil
	}
	return nil, fmt.Errorf("not found")
}
func (m *richMockCache) GetAppRuntime(ctx context.Context, appID string) (string, []byte, error) {
	return "test", m.scopeJSON, nil
}
func (m *richMockCache) Invalidate(serviceID string)       {}
func (m *richMockCache) InvalidateAppRuntime(appID string) {}

func (m *richMockCache) testSecurityRequirements() authrouting.Requirements {
	if m.securityRequirements != nil {
		return m.securityRequirements
	}
	return authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
}
