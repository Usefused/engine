package sandbox

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestMatchEndpoint_RawAndSnakeCase(t *testing.T) {
}

func TestEndpointAllowed(t *testing.T) {
	epID := uuid.New()
	sel := models.SDKSelection{EndpointIDs: []uuid.UUID{uuid.New(), epID}}

	if !endpointAllowed(sel, &fusedobject.Endpoint{ID: epID}) {
		t.Errorf("expected endpoint to be allowed")
	}
	if endpointAllowed(sel, &fusedobject.Endpoint{ID: uuid.New()}) {
		t.Errorf("did not expect a random endpoint to be allowed")
	}
}

func TestFindEndpointInScope(t *testing.T) {
	svcID := uuid.New()
	epID := uuid.New()
	newCache := func(endpointIDs []uuid.UUID) *richMockCache {
		selections := []models.SDKSelection{{ServiceID: svcID, EndpointIDs: endpointIDs}}
		scopeJSON, _ := json.Marshal(selections)
		return &richMockCache{scopeJSON: scopeJSON, obj: &fusedobject.ServiceMetadata{}, epID: endpointIDs[0]}
	}

	ctx := context.Background()
	selections := []models.SDKSelection{{ServiceID: svcID, EndpointIDs: []uuid.UUID{epID}}}

	// Found + allowed.
	match, err := findEndpointInScope(ctx, newCache([]uuid.UUID{epID}), "dummy-sdk-id", selections, "list_items")
	if err != nil || match == nil {
		t.Fatalf("expected a match, got match=%v err=%v", match, err)
	}
	if !match.allowed || match.endpoint.ID != epID {
		t.Errorf("expected allowed match for endpoint %s, got %+v", epID, match)
	}

	// Found but not allowed (scope references a different endpoint id).
	selOther := []models.SDKSelection{{ServiceID: svcID, EndpointIDs: []uuid.UUID{uuid.New()}}}
	match, err = findEndpointInScope(ctx, newCache([]uuid.UUID{uuid.New()}), "dummy-sdk-id", selOther, "list_items")
	if err != nil || match == nil {
		t.Fatalf("expected a match object, got match=%v err=%v", match, err)
	}
	if match.allowed {
		t.Errorf("expected not-allowed for out-of-scope endpoint id")
	}

	// Not found (tool name not exposed by any provider in scope).
	match, err = findEndpointInScope(ctx, newCache([]uuid.UUID{epID}), "dummy-sdk-id", selections, "nonexistent_tool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if match != nil {
		t.Errorf("expected nil match for unknown tool, got %+v", match)
	}
}
