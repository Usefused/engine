package cmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
)

type controlPrincipalLoaderStub struct {
	principal accesscontrol.ControlPrincipal
	loads     int
}

func (s *controlPrincipalLoaderStub) LoadControlPrincipal(context.Context, string) (accesscontrol.ControlPrincipal, error) {
	s.loads++
	return s.principal, nil
}

func TestControlActorMiddlewareHydratesActorAndReusesCache(t *testing.T) {
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, err := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	handler := controlActorMiddleware(authenticator)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		if !ok || actor.SubjectID != loader.principal.SubjectID || actor.WorkspaceID != loader.principal.WorkspaceID {
			t.Fatalf("request actor = %#v, %v", actor, ok)
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/workspace/services", nil)
		request.Header.Set("X-API-Key", "fsk_license")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
		}
		if !strings.Contains(response.Header().Get("Server-Timing"), "engine_authn;dur=") {
			t.Fatalf("Server-Timing = %q", response.Header().Get("Server-Timing"))
		}
	}
	if loader.loads != 1 {
		t.Fatalf("principal loads = %d, want 1", loader.loads)
	}
}

func TestControlAuthenticationAndAuthorizationReuseOneCachedSnapshot(t *testing.T) {
	principal := controlTestPrincipal()
	principal.EffectiveGrants = append(principal.EffectiveGrants, accesscontrol.Grant{
		Permission: accesscontrol.PermissionAccountRead,
		Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: principal.WorkspaceID},
	})
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, err := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	called := 0
	handler := controlActorMiddleware(authenticator)(
		controlAuthorizationMiddleware(accesscontrol.SnapshotAuthorizer{})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			called++
			w.WriteHeader(http.StatusNoContent)
		})),
	)

	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/account", nil)
		request.Header.Set("X-API-Key", "fsk_license")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("status = %d; body=%s", response.Code, response.Body.String())
		}
		if timing := strings.Join(response.Header().Values("Server-Timing"), ","); !strings.Contains(timing, "engine_authn") || !strings.Contains(timing, "engine_authz") {
			t.Fatalf("Server-Timing = %q", timing)
		}
	}
	if loader.loads != 1 || called != 2 {
		t.Fatalf("principal loads/handler calls = %d/%d, want 1/2", loader.loads, called)
	}
}

func TestControlActorMiddlewareRejectsMissingCredential(t *testing.T) {
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	handler := controlActorMiddleware(authenticator)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("protected handler must not execute")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/engine/graphql", nil))
	if response.Code != http.StatusUnauthorized || loader.loads != 0 {
		t.Fatalf("status/loads = %d/%d, want 401/0", response.Code, loader.loads)
	}
}

func TestControlActorMiddlewareAuthenticatesCookieAndEnforcesCSRF(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	cookies, err := browserauth.NewCookieManager([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatalf("NewCookieManager: %v", err)
	}
	cookieResponse := httptest.NewRecorder()
	cookies.SetSession(cookieResponse, "fsk_cookie_session", time.Now().Add(time.Hour))
	issued := cookieResponse.Result().Cookies()
	called := 0
	handler := controlActorMiddlewareWithAudit(authenticator, nil, cookies)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.Header.Get("X-API-Key") != "fsk_cookie_session" {
			t.Fatalf("internal credential projection = %q", r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	get := httptest.NewRequest(http.MethodGet, "/workspace/services", nil)
	get.AddCookie(issued[0])
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, get)
	if response.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("cookie GET status/calls = %d/%d", response.Code, called)
	}

	post := httptest.NewRequest(http.MethodPost, "/workspace/services", nil)
	post.AddCookie(issued[0])
	post.AddCookie(issued[1])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusForbidden || called != 1 {
		t.Fatalf("cookie POST without CSRF status/calls = %d/%d", response.Code, called)
	}

	post = httptest.NewRequest(http.MethodPost, "/workspace/services", nil)
	post.AddCookie(issued[0])
	post.AddCookie(issued[1])
	post.Header.Set(browserauth.CSRFHeader, issued[1].Value)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, post)
	if response.Code != http.StatusNoContent || called != 2 {
		t.Fatalf("cookie POST with CSRF status/calls = %d/%d", response.Code, called)
	}
}

func TestControlActorMiddlewareRejectsHeaderAndCookieTogether(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	cookies, _ := browserauth.NewCookieManager([]byte("01234567890123456789012345678901"))
	handler := controlActorMiddlewareWithAudit(authenticator, nil, cookies)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("ambiguous request must not execute")
	}))
	request := httptest.NewRequest(http.MethodGet, "/workspace/services", nil)
	request.Header.Set("X-API-Key", "fsk_header")
	request.AddCookie(&http.Cookie{Name: "__Host-fused_session", Value: "fsk_cookie"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || loader.loads != 0 {
		t.Fatalf("ambiguous status/loads = %d/%d", response.Code, loader.loads)
	}
}

func TestControlActorMiddlewareRejectsOrdinaryCredentialCookie(t *testing.T) {
	t.Setenv("FUSED_ENV", "production")
	principal := controlTestPrincipal()
	principal.CredentialSource = "api_key"
	loader := &controlPrincipalLoaderStub{principal: principal}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	cookies, _ := browserauth.NewCookieManager([]byte("01234567890123456789012345678901"))
	handler := controlActorMiddlewareWithAudit(authenticator, nil, cookies)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("ordinary API credential cookie must not execute")
	}))
	request := httptest.NewRequest(http.MethodGet, "/workspace/services", nil)
	request.AddCookie(&http.Cookie{Name: "__Host-fused_session", Value: "ordinary-api-key"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("ordinary cookie status = %d", response.Code)
	}
}

func TestControlActorMiddlewareExcludesRuntimeAndPublicRoutes(t *testing.T) {
	loader := &controlPrincipalLoaderStub{principal: controlTestPrincipal()}
	authenticator, _ := accesscontrol.NewAuthenticator(loader, 1, accesscontrol.AuthenticatorOptions{})
	called := 0
	handler := controlActorMiddleware(authenticator)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.WriteHeader(http.StatusNoContent)
	}))
	paths := []string{
		"/health",
		"/mcp/server/sse",
		"/webhook/example",
		"/workspace/connect/callback",
	}
	for _, path := range paths {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNoContent {
			t.Fatalf("path %s status = %d", path, response.Code)
		}
	}
	if loader.loads != 0 || called != len(paths) {
		t.Fatalf("loads/calls = %d/%d, want 0/%d", loader.loads, called, len(paths))
	}
}

func TestClassifyEngineRequest(t *testing.T) {
	tests := []struct {
		method string
		path   string
		want   engineRequestClass
	}{
		{method: http.MethodGet, path: "/health", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/managed/start", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/managed/poll", want: requestClassPublic},
		{method: http.MethodGet, path: "/auth/session", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/license/exchange", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/api-key/exchange", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/logout", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/cli/start", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/cli/poll", want: requestClassPublic},
		{method: http.MethodPost, path: "/auth/cli/approve", want: requestClassPublic},
		{method: http.MethodOptions, path: "/workspace/services", want: requestClassPublic},
		{method: http.MethodPost, path: "/mcp/example", want: requestClassRuntimeExcluded},
		{method: http.MethodPost, path: "/workspace/connect/callback", want: requestClassRuntimeExcluded},
		{method: http.MethodPost, path: "/workspace/buckets/" + uuid.NewString() + "/services/" + uuid.NewString() + "/connect/sessions", want: requestClassControl},
		{method: http.MethodGet, path: "/workspace/services", want: requestClassControl},
		{method: http.MethodGet, path: "/audit/export", want: requestClassControl},
		{method: http.MethodGet, path: "/not-an-api-route", want: requestClassUnclassified},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, nil)
		if got := classifyEngineRequest(request); got != test.want {
			t.Fatalf("%s %s class = %q, want %q", test.method, test.path, got, test.want)
		}
	}
}

func controlTestPrincipal() accesscontrol.ControlPrincipal {
	workspaceID := uuid.New()
	return accesscontrol.ControlPrincipal{
		AccountID:            uuid.New(),
		WorkspaceID:          workspaceID,
		SubjectID:            uuid.New(),
		CredentialID:         uuid.New(),
		CredentialSource:     "license_exchange",
		AuthenticationMethod: "license_key",
		Kind:                 accesscontrol.SubjectBootstrap,
		Revision:             1,
		EffectiveGrants: []accesscontrol.Grant{{
			Permission: accesscontrol.PermissionWorkspaceRead,
			Resource:   accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID},
		}},
	}
}
