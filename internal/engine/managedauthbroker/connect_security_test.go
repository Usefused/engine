package managedauthbroker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type securityCatalog struct{ app ProviderApp }

// GetProviderApp models the trusted catalogue independently from consumer request metadata.
func (c securityCatalog) GetProviderApp(context.Context, uuid.UUID, string, []byte, ...string) (ProviderApp, error) {
	return c.app, nil
}

type securityTransport func(*http.Request) (*http.Response, error)

// RoundTrip inspects every outbound request, including requests produced by redirect handling.
func (f securityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// securityResponse supplies a complete response for the real net/http client and token decoder.
func securityResponse(r *http.Request, status int, location string) *http.Response {
	return &http.Response{StatusCode: status, Request: r, Header: http.Header{"Content-Type": {"application/json"}, "Location": {location}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-access","token_type":"Bearer"}`))}
}

// securityApp creates an explicitly approved destination and credential-placement policy.
func securityApp(method fusedobject.TokenEndpointAuthMethod, media fusedobject.TokenRequestMediaType) ProviderApp {
	return ProviderApp{ClientID: "fixture-client", ClientSecret: "fixture-secret", Auth: fusedobject.AuthConfig{Type: "oauth2", PKCERequired: true, TokenEndpointAuthMethod: method, TokenRequestMediaType: media, ExtraTokenParams: map[string]string{"audience": "approved"}}, Flow: fusedobject.OAuth2FlowContract{TokenURL: "https://provider.example/token"}}
}

// securityService constructs the real broker service with an observable transport and no external network.
func securityService(t *testing.T, app ProviderApp, transport securityTransport) *ConnectService {
	t.Helper()
	client := &http.Client{Transport: transport}
	service, err := NewConnectService(securityCatalog{app}, make([]byte, 32), client)
	// Constructor failure must not masquerade as successful denial of a malicious request.
	if err != nil {
		t.Fatal(err)
	}
	// The security wrapper must not mutate a client shared by unrelated Engine services.
	if client.CheckRedirect != nil || client.Timeout != 0 {
		t.Fatal("shared HTTP client mutated")
	}
	return service
}

// maliciousGrant submits caller-owned endpoint and credential-placement metadata through both production service paths.
func maliciousGrant(t *testing.T, service *ConnectService, action string) error {
	t.Helper()
	auth := fusedobject.AuthConfig{Type: "oauth2", TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost, ExtraTokenParams: map[string]string{"audience": "attacker"}}
	flow := fusedobject.OAuth2FlowContract{TokenURL: "https://attacker.example/collect"}
	// Both endpoints formerly used these caller-supplied fields with the broker's decrypted application secret.
	if action == "exchange" {
		_, err := service.Exchange(t.Context(), uuid.New(), "OAuth", "https://consumer.example/callback", auth, flow, "fixture-code", "fixture-verifier")
		return err
	}
	_, err := service.Refresh(t.Context(), uuid.New(), "OAuth", "https://consumer.example/callback", auth, flow, "fixture-refresh")
	return err
}

// TestTokenPolicyIgnoresConsumerMetadata proves destination, credential placement and extra parameters are operator-owned.
func TestTokenPolicyIgnoresConsumerMetadata(t *testing.T) {
	for _, action := range []string{"exchange", "refresh"} {
		for _, method := range []fusedobject.TokenEndpointAuthMethod{fusedobject.TokenEndpointAuthMethodClientSecretPost, fusedobject.TokenEndpointAuthMethodClientSecretBasic} {
			for _, media := range []fusedobject.TokenRequestMediaType{fusedobject.TokenRequestMediaTypeForm, fusedobject.TokenRequestMediaTypeJSON} {
				// Each mode exercises the real token encoder with independent request accounting.
				t.Run(action+"/"+string(method)+"/"+string(media), func(t *testing.T) {
					calls := 0
					service := securityService(t, securityApp(method, media), func(r *http.Request) (*http.Response, error) {
						calls++
						assertApprovedTokenRequest(t, r, method, media)
						return securityResponse(r, 200, ""), nil
					})
					// A valid approved registration must continue working despite hostile metadata from an older consumer.
					if err := maliciousGrant(t, service, action); err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatalf("outbound requests=%d", calls)
					}
				})
			}
		}
	}
}

// assertApprovedTokenRequest verifies the full destination and secret-bearing request contract.
func assertApprovedTokenRequest(t *testing.T, r *http.Request, method fusedobject.TokenEndpointAuthMethod, media fusedobject.TokenRequestMediaType) {
	t.Helper()
	// No part of caller-selected routing or encoding may influence where the secret is sent.
	if r.URL.String() != "https://provider.example/token" || r.Header.Get("Content-Type") != string(media) {
		t.Fatal("consumer controlled token destination or encoding")
	}
	values := decodeSecurityBody(t, r, media)
	// PKCE comes from the pinned contract, even when the consumer omits it from its submitted auth settings.
	if values["grant_type"] == "authorization_code" && values["code_verifier"] != "fixture-verifier" {
		t.Fatal("pinned PKCE policy was lost")
	}
	// Extra provider parameters are equally sensitive policy and cannot be replaced by the consumer.
	if values["audience"] != "approved" {
		t.Fatal("consumer controlled token parameters")
	}
	assertSecurityCredentials(t, r, method, values)
}

// TestTokenPolicyRejectsRedirects prevents secret forwarding for every redirect class, even within the same origin.
func TestTokenPolicyRejectsRedirects(t *testing.T) {
	for _, action := range []string{"exchange", "refresh"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			for _, location := range []string{"https://attacker.example/collect", "https://provider.example/other"} {
				calls := 0
				service := securityService(t, securityApp(fusedobject.TokenEndpointAuthMethodClientSecretPost, ""), func(r *http.Request) (*http.Response, error) {
					calls++
					return securityResponse(r, status, location), nil
				})
				// Redirects must be returned as failure without allowing the HTTP client to make a second request.
				if err := maliciousGrant(t, service, action); err == nil || calls != 1 {
					t.Fatalf("%s status %d followed redirect: requests=%d err=%v", action, status, calls, err)
				}
			}
		}
	}
}

// TestTokenPolicyRejectsMissingOrUnsafeRegistration covers legacy rows and malformed operator configuration before any network use.
func TestTokenPolicyRejectsMissingOrUnsafeRegistration(t *testing.T) {
	for _, endpoint := range []string{"", "http://provider.example/token", "https://user:pass@provider.example/token", "https://provider.example/token#fragment", "/relative", "https://"} {
		for _, action := range []string{"exchange", "refresh"} {
			app := securityApp(fusedobject.TokenEndpointAuthMethodClientSecretPost, "")
			app.Flow.TokenURL = endpoint
			service := securityService(t, app, func(r *http.Request) (*http.Response, error) {
				t.Fatal("invalid registration reached network")
				return nil, nil
			})
			// Missing trusted routing must never be repaired with a caller-supplied URL.
			if err := maliciousGrant(t, service, action); err == nil {
				t.Fatalf("accepted policy %q", endpoint)
			}
		}
	}
}

// decodeSecurityBody inspects the actual token payload independently of the broker encoder.
func decodeSecurityBody(t *testing.T, r *http.Request, media fusedobject.TokenRequestMediaType) map[string]string {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	// Decode according to the operator's selected media contract to inspect the actual transmitted body.
	if media == fusedobject.TokenRequestMediaTypeJSON {
		if err := json.Unmarshal(body, &values); err != nil {
			t.Fatal(err)
		}
	} else {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		for key := range form {
			values[key] = form.Get(key)
		}
	}
	return values
}

// assertSecurityCredentials checks that the selected method never duplicates credentials in another location.
func assertSecurityCredentials(t *testing.T, r *http.Request, method fusedobject.TokenEndpointAuthMethod, values map[string]string) {
	t.Helper()
	// Basic and body credentials must follow the registered method, including removal from the alternate location.
	if method == fusedobject.TokenEndpointAuthMethodClientSecretBasic {
		user, secret, ok := r.BasicAuth()
		if !ok || user != "fixture-client" || secret != "fixture-secret" || values["client_secret"] != "" {
			t.Fatal("incorrect Basic credential placement")
		}
	} else if values["client_secret"] != "fixture-secret" || r.Header.Get("Authorization") != "" {
		t.Fatal("incorrect body credential placement")
	}
}
