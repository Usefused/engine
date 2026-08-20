package sandbox

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestResolvePhysicalOperationByNameFindsSelectAllSnapshotOperation proves
// REST classification inspects actual immutable contract contents.
func TestResolvePhysicalOperationByNameFindsSelectAllSnapshotOperation(t *testing.T) {
	appID, serviceID, versionID, endpointID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	cache, database := exactResolverTestCache(t, appID, []models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, SelectAll: true,
	}}, []fusedobject.Endpoint{{ID: endpointID, Name: "issues.get", Method: "GET"}})
	operation, found, err := resolvePhysicalOperationByName(context.Background(), cache, appID, "issues.get")
	if err != nil || !found {
		t.Fatalf("select-all lookup found=%v err=%v", found, err)
	}
	if operation.match == nil || operation.match.endpoint.ID != endpointID || database.contractBatchCalls != 1 {
		t.Fatalf("select-all operation/batch = %#v/%d", operation, database.contractBatchCalls)
	}
}

// TestResolvePhysicalOperationByNameRejectsCrossServiceDuplicate proves a
// request cannot route by the declaration order of two same-name operations.
func TestResolvePhysicalOperationByNameRejectsCrossServiceDuplicate(t *testing.T) {
	appID := uuid.New()
	firstService, secondService := uuid.New(), uuid.New()
	firstVersion, secondVersion := uuid.New(), uuid.New()
	firstEndpoint, secondEndpoint := uuid.New(), uuid.New()
	cache, _ := exactResolverTestCache(t, appID, []models.SDKSelection{
		{ServiceID: firstService, ServiceVersionID: firstVersion, EndpointIDs: []uuid.UUID{firstEndpoint}},
		{ServiceID: secondService, ServiceVersionID: secondVersion, EndpointIDs: []uuid.UUID{secondEndpoint}},
	}, []fusedobject.Endpoint{
		{ID: firstEndpoint, Name: "items.get", Method: "GET"},
		{ID: secondEndpoint, Name: "items.get", Method: "POST"},
	})
	_, found, err := resolvePhysicalOperationByName(context.Background(), cache, appID, "items.get")
	if found || !errors.Is(err, ErrPhysicalOperationAmbiguous) {
		t.Fatalf("duplicate lookup found=%v err=%v", found, err)
	}
}

// TestPreparePhysicalExecutionContextPreservesRESTTransport proves explicit
// transport survives the canonical physical boundary preparation.
func TestPreparePhysicalExecutionContextPreservesRESTTransport(t *testing.T) {
	ctx := preparePhysicalExecutionContext(context.Background(), PhysicalExecutionRequest{
		Transport: models.EngineExecutionTransportREST,
	})
	if got := executionTransportFromContext(ctx); got != models.EngineExecutionTransportREST {
		t.Fatalf("execution transport = %q", got)
	}
}
