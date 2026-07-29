package connectauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip lets token exchange tests inspect the provider request without a
// real network call, keeping secret-bearing OAuth form data inside the test.
func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

// TestRefreshAccessTokenClassifiesInvalidGrant proves the refresh boundary
// retains the one OAuth decision that requires fresh end-user consent.
func TestRefreshAccessTokenClassifiesInvalidGrant(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid_grant","error_description":"refresh token revoked"}`)),
		}, nil
	})}

	_, err := RefreshAccessToken(context.Background(), client, testAuthConfig(), testClientCredentials(), "revoked-refresh")
	var endpointErr *TokenEndpointError
	if !errors.As(err, &endpointErr) {
		t.Fatalf("RefreshAccessToken error = %T %v, want TokenEndpointError", err, err)
	}
	if endpointErr.Code != "invalid_grant" || !IsReconnectRequiredRefreshError(err) {
		t.Fatalf("unexpected refresh classification: %#v", endpointErr)
	}
	if strings.Contains(err.Error(), "refresh token revoked") {
		t.Fatalf("provider description leaked through public error: %v", err)
	}
}

// TestRefreshAccessTokenDoesNotClassifyProviderOutage prevents transient
// token-endpoint failures from forcing users through unnecessary consent.
func TestRefreshAccessTokenDoesNotClassifyProviderOutage(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
			Body:       io.NopCloser(strings.NewReader("error=temporarily_unavailable&error_description=retry")),
		}, nil
	})}

	_, err := RefreshAccessToken(context.Background(), client, testAuthConfig(), testClientCredentials(), "refresh")
	if IsReconnectRequiredRefreshError(err) {
		t.Fatalf("transient provider error must not require reconnect: %v", err)
	}
}

// TestExchangeAuthorizationCodeRequestsJSON covers GitHub's token endpoint
// behavior: without Accept: application/json it can return form-encoded data.
func TestExchangeAuthorizationCodeRequestsJSON(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}
		return jsonTokenResponse(), nil
	})}

	token, err := ExchangeAuthorizationCode(context.Background(), client, testAuthConfig(), testClientCredentials(), "code", "verifier")
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}
	if token.AccessToken != "access-token" {
		t.Fatalf("unexpected token: %#v", token)
	}
}

// TestExchangeAuthorizationCodeAcceptsFormEncodedTokenResponse proves Engine
// can consume standards-shaped OAuth responses even when a provider ignores
// the JSON Accept preference.
func TestExchangeAuthorizationCodeAcceptsFormEncodedTokenResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return formTokenResponse(), nil
	})}

	token, err := ExchangeAuthorizationCode(context.Background(), client, testAuthConfig(), testClientCredentials(), "code", "verifier")
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}
	if token.AccessToken != "access-token" || token.RefreshToken != "refresh-token" || token.ExpiresIn != 3600 || token.RefreshTokenExpiresIn != 7200 {
		t.Fatalf("unexpected form-decoded token: %#v", token)
	}
}

func TestTokenScopeMetadata(t *testing.T) {
	tests := []struct {
		name       string
		tokenScope string
		fallback   []string
		wantScopes []string
		wantSource string
	}{
		{"provider scopes", "repo user", []string{"fallback"}, []string{"repo", "user"}, "provider"},
		{"registry fallback", "", []string{"openid"}, []string{"openid"}, "registry"},
		{"request fallback", "", []string{"account:read"}, []string{"account:read"}, "request"},
		{"no scopes", "", nil, nil, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TokenScopeMetadata(TokenResponse{Scope: tt.tokenScope}, tt.fallback, tt.wantSource)
			if strings.Join(got.Scopes, ",") != strings.Join(tt.wantScopes, ",") || got.Source != tt.wantSource {
				t.Fatalf("TokenScopeMetadata = %#v, want scopes=%#v source=%q", got, tt.wantScopes, tt.wantSource)
			}
		})
	}
}

// testAuthConfig keeps provider URLs centralized so each test only varies the
// provider response behavior it is proving.
func testAuthConfig() fusedobject.AuthConfig {
	return fusedobject.AuthConfig{TokenURL: "https://provider.example/token"}
}

// testClientCredentials supplies non-secret fixtures with the same shape as
// decrypted connect config material.
func testClientCredentials() ClientCredentials {
	return ClientCredentials{ClientID: "client-id", ClientSecret: "client-secret", RedirectURI: "https://engine.example/callback"}
}

// jsonTokenResponse is the preferred provider response shape after the Accept
// header is set.
func jsonTokenResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"access_token":"access-token","token_type":"Bearer"}`)),
	}
}

// formTokenResponse mirrors GitHub-compatible OAuth form bodies so the parser
// fallback stays covered.
func formTokenResponse() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:       io.NopCloser(strings.NewReader("access_token=access-token&refresh_token=refresh-token&token_type=Bearer&expires_in=3600&refresh_token_expires_in=7200")),
	}
}
