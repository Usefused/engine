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

func (m *mockObjectCache) ConnectSDK(ctx context.Context, appID string) error {
	return nil
}

func (m *mockObjectCache) DisconnectSDK(appID string) {}

func (m *mockObjectCache) GetOrFetchServiceMetadata(ctx context.Context, appID string, serviceID string) (*fusedobject.ServiceMetadata, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.obj, nil
}

func (m *mockObjectCache) GetAppRuntime(ctx context.Context, appID string) (string, []byte, error) {
	return "test_token", []byte(`[{"service_id": "00000000-0000-0000-0000-000000000000", "endpoint_ids": ["00000000-0000-0000-0000-000000000000"]}]`), nil
}

func (m *mockObjectCache) Invalidate(serviceID string) {}

func (m *mockObjectCache) GetEndpoint(ctx context.Context, appID string, serviceID string, endpointName string) (*fusedobject.Endpoint, error) {
	return nil, nil
}

func (m *mockObjectCache) ListEndpointsForSelection(ctx context.Context, appID string, sel models.SDKSelection) ([]fusedobject.Endpoint, error) {
	return nil, nil
}
