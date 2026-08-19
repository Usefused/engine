package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// TestMapAuthConfigsPreservesTokenRequestMediaType ensures dispatch mapping
// retains the reviewed provider token-body policy without string inference.
func TestMapAuthConfigsPreservesTokenRequestMediaType(t *testing.T) {
	got := mapAuthConfigs(fusedobject.AuthConfigs{{
		Name:                    "oauth",
		Type:                    "oauth2",
		TokenRequestMediaType:   fusedobject.TokenRequestMediaTypeJSON,
		TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
	}})

	if len(got) != 1 || got[0].TokenRequestMediaType != models.TokenRequestMediaType(fusedobject.TokenRequestMediaTypeJSON) {
		t.Fatalf("token request media mapping = %#v", got)
	}
}
