package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

// TestFixturePaginationUsesEffectivePolicy keeps search guidance aligned with execution precedence and defaults.
func TestFixturePaginationUsesEffectivePolicy(t *testing.T) {
	endpoint := paginationGuidancePolicy(5)
	service := paginationGuidancePolicy(10)
	tests := []struct {
		name      string
		endpoint  *fusedobject.PaginationConfig
		service   *fusedobject.PaginationConfig
		supported bool
		boundable bool
		maxPages  int
	}{
		{name: "endpoint wins", endpoint: endpoint, service: service, supported: true, boundable: true, maxPages: 5},
		{name: "service fallback", service: service, supported: true, boundable: true, maxPages: 10},
		{name: "contract absent", supported: false},
		{name: "shared defaults", endpoint: paginationGuidancePolicy(0), supported: true, boundable: true, maxPages: paginationpolicy.DefaultMaxPages},
		{name: "one page has no lower bound", endpoint: paginationGuidancePolicy(1), supported: true, maxPages: 1},
	}
	// The table pins precedence, absence, shared defaults, and strict caller reduction together.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := fixturePagination(test.endpoint, test.service)
			// Both fields must match so unsupported operations never advertise a decorative bound.
			if got.Supported != test.supported || got.CallerBoundSupported != test.boundable || got.EngineMaxPages != test.maxPages {
				t.Fatalf("fixturePagination() = %+v, want supported=%t boundable=%t maxPages=%d", got, test.supported, test.boundable, test.maxPages)
			}
		})
	}
}

// paginationGuidancePolicy builds the smallest policy needed to verify public limit projection.
func paginationGuidancePolicy(maxPages int) *fusedobject.PaginationConfig {
	return &paginationpolicy.Config{Version: paginationpolicy.Version, Limits: paginationpolicy.Limits{MaxPages: maxPages}}
}
