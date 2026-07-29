package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

func TestMapAuthConfigsDefaultsUnnamedBearerAuthToAuthorization(t *testing.T) {
	got := mapAuthConfigs(fusedobject.AuthConfigs{{
		Type:   "http",
		Scheme: "bearer",
	}})

	if len(got) != 1 || got[0].Name != "Authorization" {
		t.Fatalf("expected unnamed bearer auth to use Authorization, got %#v", got)
	}
}

func TestMapAuthConfigsDefaultsUnnamedOAuthToAuthorization(t *testing.T) {
	got := mapAuthConfigs(fusedobject.AuthConfigs{{
		Type: "oauth2",
	}})

	if len(got) != 1 || got[0].Name != "Authorization" {
		t.Fatalf("expected unnamed oauth auth to use Authorization, got %#v", got)
	}
}

// TestFusedToIntegrationObject_EndpointPaginationWinsOverService pins the
// fallback rule from plans/plan-service-config-restructure.md item 1:
// spec-derived per-endpoint pagination always wins over the service/version
// default, since it's a more specific declaration.
func TestFusedToIntegrationObject_EndpointPaginationWinsOverService(t *testing.T) {
	svc := &fusedobject.ServiceMetadata{
		Pagination: &fusedobject.PaginationConfig{Type: "offset", RequestParam: "offset", ResponsePath: "meta.total"},
	}
	ep := fusedobject.Endpoint{
		Pagination: &fusedobject.PaginationConfig{Type: "cursor", RequestParam: "cursor", ResponsePath: "next"},
	}

	got := fusedToIntegrationObject(svc, ep)

	if got.Pagination == nil || got.Pagination.Type != "cursor" {
		t.Fatalf("expected endpoint pagination to win, got %#v", got.Pagination)
	}
}

// TestFusedToIntegrationObject_FallsBackToServicePagination covers the new
// behavior this item adds: an endpoint with no spec-derived pagination of
// its own now inherits the service/version-level execution_policy default
// instead of never auto-paginating.
func TestFusedToIntegrationObject_FallsBackToServicePagination(t *testing.T) {
	svc := &fusedobject.ServiceMetadata{
		Pagination: &fusedobject.PaginationConfig{Type: "cursor", RequestParam: "after", ResponsePath: "page.next"},
	}
	ep := fusedobject.Endpoint{}

	got := fusedToIntegrationObject(svc, ep)

	if got.Pagination == nil || got.Pagination.Type != "cursor" || got.Pagination.RequestParam != "after" || got.Pagination.ResponsePath != "page.next" {
		t.Fatalf("expected fallback to service pagination, got %#v", got.Pagination)
	}
}

// TestFusedToIntegrationObject_NoPaginationAnywhereStaysNil confirms
// non-paginated endpoints on services with no declared default still dispatch
// without triggering the pagination path.
func TestFusedToIntegrationObject_NoPaginationAnywhereStaysNil(t *testing.T) {
	got := fusedToIntegrationObject(&fusedobject.ServiceMetadata{}, fusedobject.Endpoint{})

	if got.Pagination != nil {
		t.Fatalf("expected nil pagination, got %#v", got.Pagination)
	}
}
