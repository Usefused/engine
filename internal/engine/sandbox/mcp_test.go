package sandbox

import (
	"context"
	"net/http"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestMCPSessionAuthContextBindsConnectedUserOutsideToolArguments(t *testing.T) {
	resourceID := uuid.NewString()
	headers := http.Header{}
	headers.Set("X-Fused-End-User-Ref", "customer-42")
	headers.Set("X-Fused-Resource-ID", resourceID)

	context, err := mcpSessionAuthContext(headers)
	if err != nil {
		t.Fatalf("mcpSessionAuthContext() error = %v", err)
	}
	if context["fused_end_user_ref"] != "customer-42" || context["fused_resource_id"] != resourceID {
		t.Fatalf("unexpected auth context: %#v", context)
	}
}

func TestMCPSessionAuthContextRejectsInvalidResourceID(t *testing.T) {
	headers := http.Header{"X-Fused-Resource-Id": []string{"not-a-uuid"}}
	if _, err := mcpSessionAuthContext(headers); err == nil {
		t.Fatal("expected invalid resource ID to be rejected")
	}
}

type mockObjectCache struct {
	obj *fusedobject.ServiceMetadata
	err error
}

func (m *mockObjectCache) ConnectSDK(ctx context.Context, artifactID string) error {
	return nil
}

func (m *mockObjectCache) DisconnectSDK(artifactID string) {}

func (m *mockObjectCache) GetOrFetchServiceMetadata(ctx context.Context, artifactID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.obj, nil
}

func (m *mockObjectCache) GetArtifactScope(ctx context.Context, artifactID string) (string, []byte, error) {
	return "test_token", []byte(`[{"service_id": "00000000-0000-0000-0000-000000000000", "endpoint_ids": ["00000000-0000-0000-0000-000000000000"]}]`), nil
}

func (m *mockObjectCache) Invalidate(serviceID string) {}

func (m *mockObjectCache) GetEndpoint(ctx context.Context, artifactID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	return nil, nil
}

func (m *mockObjectCache) ListEndpointsForSelection(ctx context.Context, artifactID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	return nil, nil
}
