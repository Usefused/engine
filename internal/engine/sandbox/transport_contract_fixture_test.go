package sandbox

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// TestEngineConsumesTransportContractFixture proves Engine consumes the same
// provider-neutral auth and server decisions as the control-plane clients.
func TestEngineConsumesTransportContractFixture(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "contract-fixtures", "security", "v1_transport.json")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture struct {
		AuthConfig           fusedobject.AuthConfig   `json:"auth_config"`
		OAuthAuthConfig      fusedobject.AuthConfig   `json:"oauth_auth_config"`
		SecurityRequirements authrouting.Requirements `json:"security_requirements"`
		Server               fusedobject.Server       `json:"server"`
	}
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
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
