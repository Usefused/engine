package sandbox

import (
	"context"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestCapabilityWorkspaceOperationRejectsUnselectedBinding ensures a worker call cannot select a different provider endpoint.
func TestCapabilityWorkspaceOperationRejectsUnselectedBinding(t *testing.T) {
	appID, serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{endpointID}}
	cache, _ := exactResolverTestCache(t, appID, []models.SDKSelection{selection}, []fusedobject.Endpoint{
		{ID: endpointID, Name: "items.get", Method: "GET"},
	})
	identity := auth.RuntimeIdentity{AppID: appID, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	request := CapabilityWorkspaceOperationRequest{Binding: ExactOperationBinding{
		ServiceID: serviceID, ServiceVersionID: versionID, EndpointID: uuid.New(), EndpointName: "items.get",
	}}
	_, err := ExecuteCapabilityWorkspaceOperation(context.Background(), cache, engine.NewDispatcher(), identity, request)
	// A binding mismatch must stop before request validation or provider dispatch.
	if err == nil || !strings.Contains(err.Error(), "scope") {
		t.Fatalf("unselected binding error = %v", err)
	}
}

// TestCapabilityWorkspaceOperationReusesMCPInputValidation ensures selected operations reject invalid provider inputs before dispatch.
func TestCapabilityWorkspaceOperationReusesMCPInputValidation(t *testing.T) {
	appID, serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{endpointID}}
	cache, _ := exactResolverTestCache(t, appID, []models.SDKSelection{selection}, []fusedobject.Endpoint{
		{
			ID: endpointID, Name: "items.get", Method: "GET", Path: "/items",
			Parameters:           fusedobject.Parameters{{Name: "itemId", In: "query", Required: true, Type: "string"}},
			SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}},
		},
	})
	cache.serviceMetadataCache[serviceID.String()+":"+versionID.String()].BaseURL = "https://provider.invalid"
	identity := auth.RuntimeIdentity{AppID: appID, TokenPolicy: store.AppTokenPolicy{AllowAll: true}}
	request := CapabilityWorkspaceOperationRequest{Binding: ExactOperationBinding{
		ServiceID: serviceID, ServiceVersionID: versionID, EndpointID: endpointID, EndpointName: "items.get",
	}, Input: map[string]any{}}
	_, err := ExecuteCapabilityWorkspaceOperation(context.Background(), cache, engine.NewDispatcher(), identity, request)
	// Missing required input is a local validation error; no provider URL exists in this test.
	if err == nil || !strings.Contains(err.Error(), "itemId") {
		t.Fatalf("invalid input error = %v", err)
	}
}
