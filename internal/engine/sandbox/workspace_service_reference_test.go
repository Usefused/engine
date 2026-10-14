package sandbox

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestVerifyServiceProviderReferences keeps identical imported slugs separate at the Registry-to-Engine boundary.
func TestVerifyServiceProviderReferences(t *testing.T) {
	for _, provider := range []string{"account-a", "account-b"} {
		versionID := uuid.New()
		body := `{"data":{"service":{"name":"Plunk","slug":"plunk","canonical_ref":"@` + provider + `/plunk","current_service_version":"v1","service_versions":[{"id":"` + versionID.String() + `","name":"v1"}]}}}`
		_, ref, _, actualVersion, err := decodeVerifyServiceResponse(&http.Response{Body: io.NopCloser(strings.NewReader(body))})
		// The two accounts must produce different persisted references without changing their version identities.
		if err != nil || ref != "@"+provider+"/plunk" || actualVersion != versionID {
			t.Fatalf("provider=%s ref=%s version=%s err=%v", provider, ref, actualVersion, err)
		}
	}
}

// TestRegistryWorkspaceServiceReferenceRejectsMissingNamespace prevents absent metadata from reintroducing bare collisions.
func TestRegistryWorkspaceServiceReferenceRejectsMissingNamespace(t *testing.T) {
	for _, ref := range []string{"", "plunk", "@/plunk", "@account/other", "@account/plunk/extra", "@bad account/plunk"} {
		// Invalid namespace evidence must be rejected rather than silently replaced with the bare slug.
		if _, err := registryWorkspaceServiceReference("plunk", ref); err == nil {
			t.Fatalf("accepted reference %q", ref)
		}
	}
}
