package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestResolveExactPhysicalOperationsTargetsSameNameByExactIdentityInOneQuery protects the rule that resolution uses exact service/version/endpoint identity instead of operation names alone.
func TestResolveExactPhysicalOperationsTargetsSameNameByExactIdentityInOneQuery(t *testing.T) {
	appID := uuid.New()
	firstServiceID, secondServiceID := uuid.New(), uuid.New()
	firstVersionID, secondVersionID := uuid.New(), uuid.New()
	firstEndpointID, secondEndpointID := uuid.New(), uuid.New()
	selections := []models.SDKSelection{
		{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, EndpointIDs: []uuid.UUID{firstEndpointID}},
		{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, EndpointIDs: []uuid.UUID{secondEndpointID}},
	}
	bindings := []ExactOperationBinding{
		{ServiceID: firstServiceID, ServiceVersionID: firstVersionID, EndpointID: firstEndpointID, EndpointName: "items.get"},
		{ServiceID: secondServiceID, ServiceVersionID: secondVersionID, EndpointID: secondEndpointID, EndpointName: "items.get"},
	}
	cache, database := exactResolverTestCache(t, appID, selections, []fusedobject.Endpoint{
		{ID: firstEndpointID, Name: "items.get", Method: "GET"},
		{ID: secondEndpointID, Name: "items.get", Method: "POST"},
	})

	resolved, err := ResolveExactPhysicalOperations(context.Background(), cache, appID, bindings)
	if err != nil {
		t.Fatal(err)
	}
	if database.contractBatchCalls != 1 || database.contractNameCalls != 0 || database.contractIDCalls != 0 {
		t.Fatalf("snapshot calls: batch=%d name=%d id=%d", database.contractBatchCalls, database.contractNameCalls, database.contractIDCalls)
	}
	if len(resolved) != 2 || resolved[0].match.endpoint.ID != firstEndpointID || resolved[1].match.endpoint.ID != secondEndpointID {
		t.Fatalf("same-name bindings resolved incorrectly: %#v", resolved)
	}
}

// TestResolveExactPhysicalOperationsRejectsEndpointIdentityMismatch protects the rule that resolution uses exact service/version/endpoint identity instead of operation names alone.
func TestResolveExactPhysicalOperationsRejectsEndpointIdentityMismatch(t *testing.T) {
	appID, serviceID, versionID := uuid.New(), uuid.New(), uuid.New()
	selectedID, returnedID := uuid.New(), uuid.New()
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true}
	binding := ExactOperationBinding{ServiceID: serviceID, ServiceVersionID: versionID, EndpointID: selectedID, EndpointName: "items.get"}
	cache, _ := exactResolverTestCache(t, appID, []models.SDKSelection{selection}, []fusedobject.Endpoint{
		{ID: returnedID, Name: binding.EndpointName, Method: "GET"},
	})

	if _, err := ResolveExactPhysicalOperations(context.Background(), cache, appID, []ExactOperationBinding{binding}); err == nil {
		t.Fatal("endpoint identity mismatch was accepted")
	}
}

// exactResolverTestCache implements only the exact snapshot method needed to detect name-based resolution regressions.
func exactResolverTestCache(
	t *testing.T,
	appID uuid.UUID,
	selections []models.SDKSelection,
	endpoints []fusedobject.Endpoint,
) (*LocalObjectCache, *mockCacheDB) {
	t.Helper()
	scopeJSON, err := json.Marshal(selections)
	if err != nil {
		t.Fatal(err)
	}
	database := &mockCacheDB{
		contractMetadata:  &fusedobject.ServiceMetadata{ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport()},
		contractEndpoints: endpoints,
	}
	cache := NewLocalObjectCache(database)
	cache.scopes[appID.String()] = scopeJSON
	cache.sdkVersions[appID.String()] = make(map[string]string, len(selections))
	for _, selection := range selections {
		version := selection.ServiceVersionID.String()
		cache.sdkVersions[appID.String()][selection.ServiceID.String()] = version
		cache.serviceMetadataCache[selection.ServiceID.String()+":"+version] = &fusedobject.ServiceMetadata{
			ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
			ID:                        selection.ServiceID, ServiceVersionID: selection.ServiceVersionID,
		}
	}
	return cache, database
}
