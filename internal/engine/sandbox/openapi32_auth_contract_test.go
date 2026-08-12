package sandbox

import (
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

func TestValidateAuthRuntimeContractAcceptsOpenAPI32MetadataURL(t *testing.T) {
	auth := fusedobject.AuthConfig{
		Type: "oauth2", OAuth2MetadataURL: "https://identity.example.com/.well-known/oauth-authorization-server",
		OAuth2Flows: fusedobject.OAuth2Flows{"clientCredentials": {
			TokenURL: "https://identity.example.com/token", Scopes: map[string]string{},
		}},
	}
	if err := validateAuthRuntimeContract(auth); err != nil {
		t.Fatalf("OAuth2 metadata URL rejected: %v", err)
	}
	auth.OAuth2MetadataURL = "http://identity.example.com/.well-known/oauth-authorization-server"
	if err := validateAuthRuntimeContract(auth); err == nil {
		t.Fatal("insecure OAuth2 metadata URL accepted")
	}
}

func TestValidateAuthRuntimeContractRejectsUnimplementedDeviceFlow(t *testing.T) {
	auth := fusedobject.AuthConfig{Type: "oauth2", OAuth2Flows: fusedobject.OAuth2Flows{"deviceAuthorization": {
		DeviceAuthorizationURL: "https://identity.example.com/device", TokenURL: "https://identity.example.com/token",
		Scopes: map[string]string{},
	}}}
	if err := validateAuthRuntimeContract(auth); err == nil {
		t.Fatal("device authorization flow accepted without runtime dispatch")
	}
}
