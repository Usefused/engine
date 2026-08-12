package engine

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/serverrouting"
)

func explicitAnonymousEndpoint(endpoint *models.IntegrationObject) *models.IntegrationObject {
	if endpoint.SecurityRequirements == nil {
		endpoint.SecurityRequirements = authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
	}
	return endpoint
}

func TestSelectRequestAuthChargebeeEmptyBasicPassword(t *testing.T) {
	auths := models.AuthConfigs{{Name: "chargebee", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordEmpty}}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "chargebee"}}}}
	credentials := map[string]any{"chargebee_username": "site_api_key"}
	selected, err := selectRequestAuth(auths, requirements, credentials)
	if err != nil {
		t.Fatalf("select Chargebee auth: %v", err)
	}
	req := &http.Request{Header: make(http.Header)}
	applySelectedAuth(req, selected, credentials)
	encoded := strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != "site_api_key:" {
		t.Fatalf("Chargebee Authorization decoded=%q err=%v", decoded, err)
	}
	credentials["chargebee_password"] = "must-not-be-sent"
	if _, err := selectRequestAuth(auths, requirements, credentials); err == nil {
		t.Fatal("expected non-empty Chargebee password to fail closed")
	}
}

func TestSelectRequestAuthWiseOAuthAndMTLS(t *testing.T) {
	certPEM, keyPEM := testMTLSPair(t, time.Now().Add(time.Hour))
	auths := models.AuthConfigs{
		{Name: "UserToken", Type: "oauth2", OAuth2Flows: models.OAuth2Flows{"authorizationCode": {AuthorizationURL: "https://provider.example/authorize", TokenURL: "https://provider.example/token", Scopes: map[string]string{}}}},
		{Name: "WiseMTLS", Type: "mutualTLS"},
	}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{
		{Scheme: "UserToken", Scopes: []string{}},
		{Scheme: "WiseMTLS", Scopes: []string{}},
	}}}
	credentials := map[string]any{
		"UserToken":     "access-token",
		"WiseMTLS_cert": certPEM,
		"WiseMTLS_key":  keyPEM,
	}
	selected, err := selectRequestAuth(auths, requirements, credentials)
	if err != nil || len(selected) != 2 {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
	req := &http.Request{Header: make(http.Header)}
	applySelectedAuth(req, selected, credentials)
	if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("Authorization=%q", got)
	}
	dispatcher := NewDispatcher()
	client, err := dispatcher.providerClientForAuth(selected, credentials)
	if err != nil {
		t.Fatalf("compose OAuth+mTLS provider transport: %v", err)
	}
	if client == dispatcher.client {
		t.Fatal("OAuth+mTLS execution must use a certificate-scoped transport")
	}
}

func TestSelectRequestAuthOrderedORAndExactName(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "chargebeeBasic", Type: "http", Scheme: "basic", BasicPasswordMode: authrouting.BasicPasswordRequired},
		{Name: "primaryOAuth", Type: "oauth2"},
		{Name: "adminOAuth", Type: "oauth2"},
	}
	requirements := authrouting.Requirements{
		{Schemes: []authrouting.Requirement{{Scheme: "chargebeeBasic"}}},
		{Schemes: []authrouting.Requirement{{Scheme: "primaryOAuth"}}},
		{Schemes: []authrouting.Requirement{{Scheme: "adminOAuth"}}},
	}
	credentials := map[string]any{"primaryOAuth": "primary", "adminOAuth": "admin"}
	selected, err := selectRequestAuth(auths, requirements, credentials)
	if err != nil || len(selected) != 1 || selected[0].Name != "primaryOAuth" {
		t.Fatalf("default selected=%#v err=%v", selected, err)
	}
	credentials["fused_auth_name"] = "adminOAuth"
	selected, err = selectRequestAuth(auths, requirements, credentials)
	if err != nil || len(selected) != 1 || selected[0].Name != "adminOAuth" {
		t.Fatalf("named selected=%#v err=%v", selected, err)
	}
}

func TestSelectRequestAuthExplicitAnonymousAndNilContract(t *testing.T) {
	selected, err := selectRequestAuth(nil, authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}, nil)
	if err != nil || len(selected) != 0 {
		t.Fatalf("anonymous selected=%#v err=%v", selected, err)
	}
	if _, err := selectRequestAuth(nil, nil, nil); err == nil {
		t.Fatal("nil requirements must be rejected")
	}
}

func TestDispatcherAnonymousOperationDoesNotSendAuthorization(t *testing.T) {
	dispatcher := NewDispatcher()
	dispatcher.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "" {
			t.Fatalf("anonymous operation sent Authorization header %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			Request:    request,
		}, nil
	})}
	service := &models.Service{
		Name: "public", BaseURL: "https://provider.example",
		AuthConfigs: models.AuthConfigs{{Name: "bearerAuth", Type: "http", Scheme: "bearer"}},
	}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{Name: "health", Method: http.MethodGet, Path: "/health"})
	status, err := dispatcher.ExecuteStream(context.Background(), service, operation, nil, map[string]any{"bearerAuth": "stored-token"}, nil, &mockStream{})
	if err != nil || status != http.StatusOK {
		t.Fatalf("anonymous dispatch status=%d err=%v", status, err)
	}
}

func TestApplySelectedAuthComposesHeaderQueryAndCookie(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "headerKey", Type: "apiKey", Location: "header", KeyName: "X-API-Key"},
		{Name: "queryKey", Type: "apiKey", Location: "query", KeyName: "api_key"},
		{Name: "cookieKey", Type: "apiKey", Location: "cookie", KeyName: "session"},
	}
	req := &http.Request{Header: make(http.Header), URL: &url.URL{RawQuery: "existing=1"}}
	applySelectedAuth(req, auths, map[string]any{"headerKey": "h", "queryKey": "q", "cookieKey": "c"})
	if req.Header.Get("X-API-Key") != "h" || req.URL.Query().Get("api_key") != "q" {
		t.Fatalf("header=%q query=%q", req.Header.Get("X-API-Key"), req.URL.RawQuery)
	}
	cookie, err := req.Cookie("session")
	if err != nil || cookie.Value != "c" {
		t.Fatalf("cookie=%#v err=%v", cookie, err)
	}
}

func TestBasicPasswordModes(t *testing.T) {
	tests := []struct {
		name        string
		mode        authrouting.BasicPasswordMode
		credentials map[string]any
		want        bool
	}{
		{name: "required", mode: authrouting.BasicPasswordRequired, credentials: map[string]any{"basic_username": "u", "basic_password": "p"}, want: true},
		{name: "required missing", mode: authrouting.BasicPasswordRequired, credentials: map[string]any{"basic_username": "u"}},
		{name: "optional absent", mode: authrouting.BasicPasswordOptional, credentials: map[string]any{"basic_username": "u"}, want: true},
		{name: "optional empty", mode: authrouting.BasicPasswordOptional, credentials: map[string]any{"basic_username": "u", "basic_password": ""}, want: true},
		{name: "empty absent", mode: authrouting.BasicPasswordEmpty, credentials: map[string]any{"basic_username": "u"}, want: true},
		{name: "empty rejects value", mode: authrouting.BasicPasswordEmpty, credentials: map[string]any{"basic_username": "u", "basic_password": "p"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			auth := models.AuthConfig{Name: "basic", Type: "http", Scheme: "basic", BasicPasswordMode: test.mode}
			if got := basicAuthSatisfied(auth, test.credentials); got != test.want {
				t.Fatalf("basicAuthSatisfied=%v want %v", got, test.want)
			}
		})
	}
}

func TestSelectRequestAuthRequiresPinnedOAuth2Flow(t *testing.T) {
	auths := models.AuthConfigs{{Name: "oauth", Type: "oauth2", OAuth2Flows: map[string]models.OAuth2FlowContract{
		"authorizationCode": {AuthorizationURL: "https://provider.example/authorize", TokenURL: "https://provider.example/token", Scopes: map[string]string{"read": "Read"}},
		"clientCredentials": {TokenURL: "https://provider.example/token", Scopes: map[string]string{}},
	}}}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "oauth"}}}}
	credentials := map[string]any{"oauth": "token"}
	if _, err := selectRequestAuth(auths, requirements, credentials); err == nil || err.(*AuthRoutingError).Code != "oauth2_flow_required" {
		t.Fatalf("missing profile flow error = %v", err)
	}
	credentials["fused_oauth2_flow"] = "authorizationCode"
	selected, err := selectRequestAuth(auths, requirements, credentials)
	if err != nil || selected[0].SelectedOAuth2Flow == nil || selected[0].SelectedOAuth2Flow.Scopes["read"] != "Read" {
		t.Fatalf("selected=%#v err=%v", selected, err)
	}
}

// TestSelectRequestAuthSkipsUnselectedOAuthFlow proves an explicit bearer
// choice reaches its provider alternative even when an ambiguous OAuth scheme
// appears first in source order.
func TestSelectRequestAuthSkipsUnselectedOAuthFlow(t *testing.T) {
	auths := models.AuthConfigs{
		{Name: "oauth2", Type: "oauth2", OAuth2Flows: models.OAuth2Flows{
			"authorizationCode": {AuthorizationURL: "https://auth.example/authorize", TokenURL: "https://auth.example/token"},
			"clientCredentials": {TokenURL: "https://auth.example/token"},
		}},
		{Name: "privateAccessToken", Type: "http", Scheme: "bearer"},
	}
	requirements := authrouting.Requirements{
		{Schemes: []authrouting.Requirement{{Scheme: "oauth2"}}},
		{Schemes: []authrouting.Requirement{{Scheme: "privateAccessToken"}}},
	}
	credentials := map[string]any{
		"fused_auth_type": "bearer", "fused_auth_name": "privateAccessToken",
		"privateAccessToken": "fake-provider-token",
	}

	selected, err := selectRequestAuth(auths, requirements, credentials)
	if err != nil {
		t.Fatalf("selectRequestAuth: %v", err)
	}
	if len(selected) != 1 || selected[0].Name != "privateAccessToken" {
		t.Fatalf("selected auth = %#v", selected)
	}
}

func TestOAuth1SigningIsProviderNeutral(t *testing.T) {
	auth := models.AuthConfig{Name: "signed", Type: "oauth1", Strategy: &models.AuthRuntimeStrategy{
		Kind: "oauth1_signature", OAuth1: &models.OAuth1Strategy{SignatureMethod: "hmac_sha256", ParameterLocation: "authorization_header"},
	}}
	req, _ := http.NewRequest(http.MethodPost, "https://vendor.example/items?include=all", strings.NewReader("name=value"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	err := applySelectedAuthChecked(req, models.AuthConfigs{auth}, map[string]any{
		"signed_consumer_key": "consumer", "signed_consumer_secret": "secret", "signed_token": "token", "signed_token_secret": "token-secret",
	})
	header := req.Header.Get("Authorization")
	if err != nil || !strings.HasPrefix(header, "OAuth ") {
		t.Fatalf("OAuth1 header=%q err=%v", header, err)
	}
	for _, field := range []string{"oauth_consumer_key", "oauth_nonce", "oauth_signature_method", "oauth_signature", "oauth_timestamp", "oauth_token"} {
		if !strings.Contains(header, field+"=") {
			t.Fatalf("OAuth1 header missing %s: %q", field, header)
		}
	}
}

func TestDigestChallengeRetriesWithoutProviderBranch(t *testing.T) {
	auth := models.AuthConfig{Name: "login", Type: "http", Scheme: "digest", Strategy: &models.AuthRuntimeStrategy{
		Kind: "http_challenge", Challenge: &models.HTTPChallengeStrategy{Scheme: "digest"},
	}}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if requests == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": {`Digest realm="area", nonce="abc", algorithm=SHA-256, qop="auth"`}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		}
		if !strings.HasPrefix(req.Header.Get("Authorization"), "Digest ") || !strings.Contains(req.Header.Get("Authorization"), "response=") {
			t.Fatalf("digest Authorization = %q", req.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})}
	req, _ := http.NewRequest(http.MethodGet, "https://vendor.example/private", nil)
	initial, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	final, err := retryHTTPChallenge(context.Background(), client, req, initial, models.AuthConfigs{auth}, map[string]any{"login_username": "user", "login_password": "pass"})
	if err != nil || final.StatusCode != http.StatusOK || requests != 2 {
		t.Fatalf("status=%v requests=%d err=%v", final.StatusCode, requests, err)
	}
}

func TestSecurityAlternativeSelectsDeclaredMTLSServer(t *testing.T) {
	service := &models.Service{BaseURL: "https://api.example/v1", ServiceBaseURL: "https://api.example/v1", Servers: models.Servers{{URL: "https://mtls.example/{region}", Variables: []serverrouting.Variable{{Name: "region", Default: stringPointer("eu")}}}}}
	operation := &models.IntegrationObject{SecurityRequirements: authrouting.Requirements{{
		Schemes: []authrouting.Requirement{{Scheme: "cert"}}, ServerSelection: &authrouting.ServerSelection{Scheme: "cert", ServerURL: "https://mtls.example/{region}"},
	}}}
	if err := applySelectedSecurityServer(service, operation, models.AuthConfigs{{Name: "cert", Type: "mutualTLS"}}, nil); err != nil {
		t.Fatal(err)
	}
	if service.BaseURL != "https://mtls.example/eu" || service.ServerSource != "operation" {
		t.Fatalf("service server=%q source=%q", service.BaseURL, service.ServerSource)
	}
}

func stringPointer(value string) *string { return &value }
