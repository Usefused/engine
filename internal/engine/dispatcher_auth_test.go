package engine

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
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
	auths := models.AuthConfigs{
		{Name: "wiseOAuth", Type: "oauth2"},
		{Name: "wiseMTLS", Type: "mutualTLS"},
	}
	requirements := authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: "wiseOAuth"}, {Scheme: "wiseMTLS"}}}}
	credentials := map[string]any{
		"wiseOAuth":     "access-token",
		"wiseMTLS_cert": "certificate",
		"wiseMTLS_key":  "private-key",
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
