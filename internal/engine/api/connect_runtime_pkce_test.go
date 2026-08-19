package api

import (
	"net/url"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// TestBuildConnectAuthorizeURLEmitsPKCEOnlyWhenRequired ensures reviewed
// provider metadata, rather than Engine defaults, controls PKCE parameters.
func TestBuildConnectAuthorizeURLEmitsPKCEOnlyWhenRequired(t *testing.T) {
	tests := []struct {
		name     string
		required bool
	}{{name: "disabled"}, {name: "required", required: true}}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := fusedobject.AuthConfig{
				PKCERequired: test.required,
				ExtraAuthParams: map[string]string{
					"code_challenge": "metadata-challenge", "code_challenge_method": "plain",
				},
			}
			raw, err := buildConnectAuthorizeURL(auth, connectTestOAuthFlow(), nil, connectClientCredentials{}, "state", "challenge", "nonce")
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			values := parsed.Query()
			if (values.Get("code_challenge") != "") != test.required || (values.Get("code_challenge_method") != "") != test.required {
				t.Fatalf("PKCE required=%t query=%#v", test.required, values)
			}
			if test.required && (values.Get("code_challenge") != "challenge" || values.Get("code_challenge_method") != "S256") {
				t.Fatalf("PKCE metadata overrode Engine values: %#v", values)
			}
		})
	}
}
