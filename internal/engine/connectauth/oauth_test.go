package connectauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

	_, err := RefreshAccessToken(context.Background(), client, testAuthConfig(), testOAuth2Flow(), testClientCredentials(), "revoked-refresh")
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

	_, err := RefreshAccessToken(context.Background(), client, testAuthConfig(), testOAuth2Flow(), testClientCredentials(), "refresh")
	if IsReconnectRequiredRefreshError(err) {
		t.Fatalf("transient provider error must not require reconnect: %v", err)
	}
}

// TestExchangeAuthorizationCodeRequestsJSON keeps provider response handling
// deterministic without assuming its default representation.
func TestExchangeAuthorizationCodeRequestsJSON(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Accept"); got != "application/json" {
			t.Fatalf("Accept = %q, want application/json", got)
		}
		return jsonTokenResponse(), nil
	})}

	token, err := ExchangeAuthorizationCode(context.Background(), client, testAuthConfig(), testOAuth2Flow(), testClientCredentials(), "code", "verifier")
	if err != nil {
		t.Fatalf("ExchangeAuthorizationCode: %v", err)
	}
	if token.AccessToken != "access-token" {
		t.Fatalf("unexpected token: %#v", token)
	}
}

func TestTokenGrantsUseExactEndpointAuthMethod(t *testing.T) {
	grants := []struct {
		name string
		call func(context.Context, *http.Client, fusedobject.AuthConfig, ClientCredentials) (TokenResponse, error)
		key  string
		want string
	}{
		{name: "authorization code", call: func(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds ClientCredentials) (TokenResponse, error) {
			return ExchangeAuthorizationCode(ctx, client, auth, testOAuth2Flow(), creds, "authorization-code", "pkce-verifier")
		}, key: "code", want: "authorization-code"},
		{name: "refresh", call: func(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds ClientCredentials) (TokenResponse, error) {
			return RefreshAccessToken(ctx, client, auth, testOAuth2Flow(), creds, "refresh-token")
		}, key: "refresh_token", want: "refresh-token"},
	}
	methods := []fusedobject.TokenEndpointAuthMethod{
		fusedobject.TokenEndpointAuthMethodClientSecretBasic,
		fusedobject.TokenEndpointAuthMethodClientSecretPost,
	}

	for _, grant := range grants {
		for _, method := range methods {
			t.Run(grant.name+"/"+string(method), func(t *testing.T) {
				client := tokenRequestAssertionClient(t, method, grant.key, grant.want, true)
				auth := testAuthConfig()
				auth.TokenEndpointAuthMethod = method
				auth.ExtraTokenParams = map[string]string{
					"client_id": "metadata-id", "client_secret": "metadata-secret", "grant_type": "metadata-grant",
					"redirect_uri": "https://metadata.example/callback", "audience": "payments",
				}
				if _, err := grant.call(context.Background(), client, auth, testClientCredentials()); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTokenGrantsRejectUnknownOrEmptyMethodBeforeHTTP(t *testing.T) {
	tests := []struct {
		name     string
		authType string
		method   fusedobject.TokenEndpointAuthMethod
	}{
		{name: "empty", authType: "oauth2"},
		{name: "legacy Basic", authType: "oauth2", method: "basic"},
		{name: "legacy body", authType: "oauth2", method: "body"},
		{name: "unknown", authType: "oauth2", method: "unexpected"},
		{name: "non OAuth", authType: "openIdConnect", method: fusedobject.TokenEndpointAuthMethodClientSecretPost},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests.Add(1)
				return jsonTokenResponse(), nil
			})}
			auth := testAuthConfig()
			auth.Type = test.authType
			auth.TokenEndpointAuthMethod = test.method
			_, err := RefreshAccessToken(context.Background(), client, auth, testOAuth2Flow(), testClientCredentials(), "refresh-token")
			if err == nil || requests.Load() != 0 {
				t.Fatalf("error/requests = %v/%d", err, requests.Load())
			}
			if strings.Contains(err.Error(), "client-secret") || strings.Contains(err.Error(), "refresh-token") {
				t.Fatalf("credential leaked in error: %v", err)
			}
		})
	}
}

func TestTokenGrantAnnotatesExistingSpanWithBoundedMethodAndOutcome(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "existing.connect.span")
	auth := testAuthConfig()
	_, err := ExchangeAuthorizationCode(ctx, tokenRequestAssertionClient(t, auth.TokenEndpointAuthMethod, "code", "authorization-code", false), auth, testOAuth2Flow(), testClientCredentials(), "authorization-code", "pkce-verifier")
	if err != nil {
		t.Fatal(err)
	}
	span.End()

	attributes := recordedTokenSpanAttributes(t, recorder)
	if attributes["oauth.token_endpoint_auth_method"] != "client_secret_post" || attributes["oauth.token_request_outcome"] != "success" {
		t.Fatalf("bounded OAuth attributes = %#v", attributes)
	}
	for _, forbidden := range []string{"client-secret", "authorization-code", "provider.example", "client-id"} {
		if strings.Contains(strings.Join(mapValues(attributes), " "), forbidden) {
			t.Fatalf("trace attributes contain forbidden value %q", forbidden)
		}
	}
}

func TestRejectedTokenGrantAnnotatesExistingSpanWithoutRawMethod(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	ctx, span := provider.Tracer("test").Start(context.Background(), "existing.refresh.span")
	auth := testAuthConfig()
	auth.TokenEndpointAuthMethod = "client-secret-marker"
	_, err := RefreshAccessToken(ctx, http.DefaultClient, auth, testOAuth2Flow(), testClientCredentials(), "refresh-token")
	if err == nil {
		t.Fatal("expected invalid runtime method")
	}
	span.End()

	attributes := recordedTokenSpanAttributes(t, recorder)
	if attributes["oauth.token_endpoint_auth_method"] != "invalid" || attributes["oauth.token_request_outcome"] != "rejected" {
		t.Fatalf("bounded OAuth attributes = %#v", attributes)
	}
	if strings.Contains(strings.Join(mapValues(attributes), " "), "client-secret-marker") {
		t.Fatal("raw invalid method leaked into trace attributes")
	}
}

// TestExchangeAuthorizationCodeAcceptsFormEncodedTokenResponse proves Engine
// can consume standards-shaped OAuth responses even when a provider ignores
// the JSON Accept preference.
func TestExchangeAuthorizationCodeAcceptsFormEncodedTokenResponse(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return formTokenResponse(), nil
	})}

	token, err := ExchangeAuthorizationCode(context.Background(), client, testAuthConfig(), testOAuth2Flow(), testClientCredentials(), "code", "verifier")
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
	return fusedobject.AuthConfig{
		Type: "oauth2", OAuth2Flows: fusedobject.OAuth2Flows{"authorizationCode": testOAuth2Flow()},
		TokenEndpointAuthMethod: fusedobject.TokenEndpointAuthMethodClientSecretPost,
	}
}

func testOAuth2Flow() fusedobject.OAuth2FlowContract {
	return fusedobject.OAuth2FlowContract{TokenURL: "https://provider.example/token", Scopes: map[string]string{}}
}

func tokenRequestAssertionClient(t *testing.T, method fusedobject.TokenEndpointAuthMethod, grantKey, grantValue string, wantAudience bool) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertTokenGrantForm(t, req.PostForm, method, grantKey, grantValue, wantAudience)
		switch method {
		case fusedobject.TokenEndpointAuthMethodClientSecretBasic:
			assertClientSecretBasicRequest(t, req)
		case fusedobject.TokenEndpointAuthMethodClientSecretPost:
			assertClientSecretPostRequest(t, req)
		}
		return jsonTokenResponse(), nil
	})}
}

func assertTokenGrantForm(t *testing.T, form map[string][]string, method fusedobject.TokenEndpointAuthMethod, grantKey, grantValue string, wantAudience bool) {
	t.Helper()
	wantGrant, wantFields := "refresh_token", 3
	if grantKey == "code" {
		wantGrant, wantFields = "authorization_code", 4
		if firstFormValue(form, "code_verifier") != "pkce-verifier" {
			t.Fatal("code_verifier is not present")
		}
	}
	if method == fusedobject.TokenEndpointAuthMethodClientSecretPost {
		wantFields += 2
	}
	if wantAudience {
		wantFields++
	}
	wantAudienceValue := ""
	if wantAudience {
		wantAudienceValue = "payments"
	}
	if firstFormValue(form, grantKey) != grantValue || firstFormValue(form, "grant_type") != wantGrant ||
		firstFormValue(form, "redirect_uri") != "https://engine.example/callback" || firstFormValue(form, "audience") != wantAudienceValue || len(form) != wantFields {
		t.Fatal("token grant form is invalid")
	}
}

func firstFormValue(form map[string][]string, key string) string {
	if len(form[key]) == 0 {
		return ""
	}
	return form[key][0]
}

func assertClientSecretBasicRequest(t *testing.T, req *http.Request) {
	t.Helper()
	clientID, clientSecret, ok := req.BasicAuth()
	if !ok || clientID != "client-id" || clientSecret != "client-secret" || req.PostForm.Has("client_id") || req.PostForm.Has("client_secret") {
		t.Fatal("client_secret_basic request shape is invalid")
	}
}

func assertClientSecretPostRequest(t *testing.T, req *http.Request) {
	t.Helper()
	_, _, hasBasic := req.BasicAuth()
	if hasBasic || req.Header.Get("Authorization") != "" || req.PostForm.Get("client_id") != "client-id" || req.PostForm.Get("client_secret") != "client-secret" {
		t.Fatal("client_secret_post request shape is invalid")
	}
}

func mapValues(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func recordedTokenSpanAttributes(t *testing.T, recorder *tracetest.SpanRecorder) map[string]string {
	t.Helper()
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d", len(ended))
	}
	attributes := make(map[string]string, len(ended[0].Attributes()))
	for _, item := range ended[0].Attributes() {
		attributes[string(item.Key)] = item.Value.AsString()
	}
	return attributes
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
