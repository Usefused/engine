package sandbox

import (
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/testcontract"
)

// TestEngineConsumesTransportContract proves Engine consumes provider-neutral
// auth and server decisions without importing control-plane test data.
func TestEngineConsumesTransportContract(t *testing.T) {
	fixture := testcontract.Transport()
	if fixture.AuthConfig.Name != "emptyPasswordBasic" || fixture.AuthConfig.BasicPasswordMode != authrouting.BasicPasswordEmpty {
		t.Fatalf("basic wire changed: %#v", fixture.AuthConfig)
	}
	if fixture.OAuthAuthConfig.TokenEndpointAuthMethod != fusedobject.TokenEndpointAuthMethodClientSecretBasic {
		t.Fatalf("OAuth token endpoint auth method changed: %#v", fixture.OAuthAuthConfig)
	}
	wantFirst := []string{"multiFlowOAuth", "clientCertificate"}
	var gotFirst []string
	for _, requirement := range fixture.SecurityRequirements[0].Schemes {
		gotFirst = append(gotFirst, requirement.Scheme)
	}
	if !reflect.DeepEqual(gotFirst, wantFirst) || len(fixture.SecurityRequirements[2].Schemes) != 0 {
		t.Fatalf("security wire changed: %#v", fixture.SecurityRequirements)
	}
	if fixture.Server.URL != "https://{tenant}.example.test/{api-version}" || len(fixture.Server.Variables) != 2 || fixture.Server.Variables[0].Name != "tenant" {
		t.Fatalf("server wire changed: %#v", fixture.Server)
	}
}
