package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const maxOAuthFormBytes = 8 << 10

// OAuthProviderService is the narrow surface the HTTP and GraphQL layers need
// from *oauthprovider.Service, matching the ManagedLoginService/CLILoginService
// convention so cmd/engine only depends on this package's own interface.
type OAuthProviderService interface {
	CreateClient(ctx context.Context, actor accesscontrol.Actor, input oauthprovider.CreateClientInput) (oauthprovider.CreateClientResult, error)
	ListClients(ctx context.Context) ([]store.OAuthClient, error)
	RevokeClient(ctx context.Context, actor accesscontrol.Actor, id uuid.UUID) error
	Authorize(ctx context.Context, actor accesscontrol.Actor, req oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error)
	Consent(ctx context.Context, actor accesscontrol.Actor, req oauthprovider.ConsentRequest) (string, error)
	Token(ctx context.Context, req oauthprovider.TokenRequest) (oauthprovider.TokenResponse, error)
	Revoke(ctx context.Context, req oauthprovider.RevokeRequest) error
	RegisterClient(ctx context.Context, req oauthprovider.RegisterClientRequest) (oauthprovider.RegisterClientResult, error)
	ListConnectedApps(ctx context.Context, actor accesscontrol.Actor) ([]store.OAuthConnectedApp, error)
	RevokeConnectedApp(ctx context.Context, actor accesscontrol.Actor, clientID uuid.UUID) error
}

// MountOAuthProviderRoutes wires Engine's OAuth2 authorization-server surface:
// the browser-facing authorize/consent pair (session-cookie authenticated,
// same as every other browser-owned Engine page), the third-party-client-
// facing token/revoke endpoints (client-credential authenticated, no Fused
// session involved at all), the end-user "connected apps" self-service pair
// (ordinary control-plane REST, actor already resolved by the shared
// middleware), and the RFC 8414 discovery document.
func MountOAuthProviderRoutes(router chi.Router, service OAuthProviderService, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager, loginPath, publicURL string) {
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Get("/oauth/authorize", oauthAuthorizeHandler(service, s, sessions, cookies, loginPath))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Post("/oauth/authorize/consent", oauthConsentHandler(service, s, sessions, cookies))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).
		Post("/oauth/token", oauthTokenHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).
		Post("/oauth/revoke", oauthRevokeHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Post("/oauth/register", oauthRegisterHandler(service))
	router.Get("/oauth/connected-apps", oauthConnectedAppsListHandler(service))
	router.Delete("/oauth/connected-apps/{client_id}", oauthConnectedAppsRevokeHandler(service))
	router.Get("/.well-known/oauth-authorization-server", oauthMetadataHandler(publicURL))
}

// -- GET /oauth/authorize --

// oauthAuthorizeHandler starts consent or reports a policy denial in the browser.
func oauthAuthorizeHandler(service OAuthProviderService, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager, loginPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		if service == nil || sessions == nil || cookies == nil {
			renderOAuthErrorPage(r, s, w, http.StatusServiceUnavailable, "OAuth is not available on this Engine.")
			return
		}
		actor, ok := resolveOAuthBrowserActor(r, sessions, cookies)
		if !ok {
			http.Redirect(w, r, oauthLoginRedirectURL(loginPath, r), http.StatusFound)
			return
		}
		req := parseOAuthAuthorizeQuery(r)
		result, err := service.Authorize(r.Context(), actor, req)
		if err != nil {
			if target, redirectable := oauthRedirectableAuthorizeError(err, req); redirectable {
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, oauthConsentFailureMessage(err))
			return
		}
		if !result.RequiresConsent {
			http.Redirect(w, r, result.RedirectURL, http.StatusFound)
			return
		}
		renderOAuthConsentPage(w, r, s, result, req)
	}
}

func resolveOAuthBrowserActor(r *http.Request, sessions BrowserSessionService, cookies *browserauth.CookieManager) (accesscontrol.Actor, bool) {
	rawCredential, source, err := browserauth.CredentialFromRequest(r, cookies)
	if err != nil || source != browserauth.CredentialSourceCookie || rawCredential == "" {
		return accesscontrol.Actor{}, false
	}
	actor, err := sessions.Session(r.Context(), rawCredential)
	if err != nil {
		return accesscontrol.Actor{}, false
	}
	return actor, true
}

func oauthLoginRedirectURL(loginPath string, r *http.Request) string {
	if loginPath == "" {
		loginPath = "/login"
	}
	parsed, err := url.Parse(loginPath)
	if err != nil {
		return loginPath
	}
	query := parsed.Query()
	query.Set("next", r.URL.RequestURI())
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func parseOAuthAuthorizeQuery(r *http.Request) oauthprovider.AuthorizeRequest {
	query := r.URL.Query()
	return oauthprovider.AuthorizeRequest{
		ClientID: query.Get("client_id"), RedirectURI: query.Get("redirect_uri"),
		ResponseType: query.Get("response_type"), Scope: splitOAuthScope(query.Get("scope")),
		State: query.Get("state"), CodeChallenge: query.Get("code_challenge"),
		CodeChallengeMethod: query.Get("code_challenge_method"),
	}
}

func splitOAuthScope(value string) []string {
	return strings.Fields(value)
}

// oauthRedirectableAuthorizeError only redirects an error the client can act
// on once redirect_uri itself is already known-good (RFC 6749 4.1.2.1): an
// invalid or unregistered client/redirect_uri must never be used as a
// redirect target, so those failures always render an error page instead.
func oauthRedirectableAuthorizeError(err error, req oauthprovider.AuthorizeRequest) (string, bool) {
	if !errors.Is(err, oauthprovider.ErrInvalidScope) && !errors.Is(err, oauthprovider.ErrAccessDenied) {
		return "", false
	}
	return oauthErrorRedirect(req.RedirectURI, oauthErrorCode(err), req.State)
}

func oauthErrorRedirect(redirectURI, code, state string) (string, bool) {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	query := parsed.Query()
	query.Set("error", code)
	if state != "" {
		query.Set("state", state)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), true
}

func oauthErrorCode(err error) string {
	switch {
	case errors.Is(err, oauthprovider.ErrInvalidScope):
		return "invalid_scope"
	case errors.Is(err, oauthprovider.ErrAccessDenied):
		return "access_denied"
	default:
		return "server_error"
	}
}

// -- POST /oauth/authorize/consent --

// oauthConsentHandler authorizes a browser-approved grant and explains quota failures.
func oauthConsentHandler(service OAuthProviderService, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		if service == nil || sessions == nil || cookies == nil {
			renderOAuthErrorPage(r, s, w, http.StatusServiceUnavailable, "OAuth is not available on this Engine.")
			return
		}
		actor, ok := resolveOAuthBrowserActor(r, sessions, cookies)
		if !ok {
			renderOAuthErrorPage(r, s, w, http.StatusUnauthorized, "Your session has expired. Sign in again and retry from the original link.")
			return
		}
		// The consent form is a plain browser POST, not an SPA fetch call, so it
		// cannot carry the double-submit CSRF header; origin verification is the
		// same mitigation already used for the other non-SPA browser endpoint
		// (managed login start).
		if !cookies.ValidateSameOrigin(r) {
			renderOAuthErrorPage(r, s, w, http.StatusForbidden, "This request could not be verified. Return to the application and try again.")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
		if err := r.ParseForm(); err != nil {
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, "The consent request was invalid.")
			return
		}
		req := oauthprovider.ConsentRequest{
			ClientID: r.PostForm.Get("client_id"), RedirectURI: r.PostForm.Get("redirect_uri"),
			Scope: splitOAuthScope(r.PostForm.Get("scope")), State: r.PostForm.Get("state"),
			CodeChallenge: r.PostForm.Get("code_challenge"), CodeChallengeMethod: r.PostForm.Get("code_challenge_method"),
		}
		if r.PostForm.Get("decision") != "allow" {
			target, ok := oauthErrorRedirect(req.RedirectURI, "access_denied", req.State)
			if !ok {
				renderOAuthErrorPage(r, s, w, http.StatusBadRequest, "The connection request was invalid.")
				return
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		redirectURL, err := service.Consent(r.Context(), actor, req)
		if err != nil {
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, oauthConsentFailureMessage(err))
			return
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
	}
}

// oauthConsentFailureMessage gives users a way to free capacity without exposing internal errors.
func oauthConsentFailureMessage(err error) string {
	// A full quota is actionable through the user's existing Connected Apps page.
	if errors.Is(err, store.ErrOAuthDynamicClientLimit) {
		return fmt.Sprintf("You already have %d active temporary OAuth clients. Disconnect one in Access → Connected Apps, or wait for it to expire, then try again.", store.MaxActiveOAuthDynamicClientsPerUser)
	}
	return "The connection could not be authorized. Return to the application and try again."
}

// -- Consent page rendering (server-rendered, mirrors connect_input_handlers.go) --

type oauthConsentPage struct {
	Branding            hostedConnectBranding
	ClientName          string
	ScopeLabels         []string
	FormAction          string
	ClientID            string
	RedirectURI         string
	Scope               string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
}

var oauthConsentTemplate = parseHostedConnectTemplate("oauth-consent", `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Authorize {{.ClientName}} · {{.Branding.DisplayName}}</title>
  {{template "hosted-connect-shell-style"}}
  <style>
    .oauth-scopes{margin:1.25rem 0 0;padding:0;list-style:none}
    .oauth-scopes li{padding:.55rem 0;border-top:1px solid #f0edf2;font-size:.9rem;color:#15121c}
    .oauth-scopes li:first-child{border-top:none}
    .oauth-actions{display:flex;gap:.75rem;margin-top:1.5rem}
    .oauth-actions button{flex:1;min-height:46px;border-radius:.7rem;font:inherit;font-size:1rem;font-weight:750;cursor:pointer}
    .oauth-allow{border:1px solid rgba(21,18,28,.18);background:var(--connect-accent);color:var(--connect-accent-foreground)}
    .oauth-deny{border:1px solid #b8b1bc;background:#fff;color:#15121c}
  </style>
</head>
<body><main style="--connect-accent:{{.Branding.PrimaryColor}};--connect-accent-foreground:{{.Branding.AccentForeground}}">
  <header class="connect-brand">{{if .Branding.LogoURL}}<img class="connect-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></header>
  <p class="connect-eyebrow">Authorize application</p>
  <h1>{{.ClientName}} wants to access your account</h1>
  <p class="connect-copy">This application will be able to:</p>
  <ul class="oauth-scopes">{{range .ScopeLabels}}<li>{{.}}</li>{{end}}</ul>
  <form method="post" action="{{.FormAction}}">
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="scope" value="{{.Scope}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
    <div class="oauth-actions">
      <button class="oauth-deny" type="submit" name="decision" value="deny">Deny</button>
      <button class="oauth-allow" type="submit" name="decision" value="allow">Allow</button>
    </div>
  </form>
</main></body></html>`)

func renderOAuthConsentPage(w http.ResponseWriter, r *http.Request, s store.Store, result oauthprovider.AuthorizeResult, req oauthprovider.AuthorizeRequest) {
	branding := loadHostedConnectBranding(r.Context(), s)
	labels := make([]string, 0, len(result.Scope))
	for _, scope := range result.Scope {
		labels = append(labels, oauthScopeDescription(scope))
	}
	page := oauthConsentPage{
		Branding: branding, ClientName: result.Client.Name, ScopeLabels: labels,
		FormAction: "/oauth/authorize/consent", ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		Scope: strings.Join(result.Scope, " "), State: req.State,
		CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = oauthConsentTemplate.Execute(w, page)
}

var oauthScopeDescriptions = map[string]string{
	"workspace.read":            "View workspace details",
	"workspace.update":          "Update workspace settings",
	"service.read":              "View connected services",
	"service.consume":           "Use connected services",
	"service.manage":            "Manage connected services",
	"bucket.read":               "View buckets",
	"bucket.values.read":        "Read bucket values",
	"bucket.use":                "Use buckets",
	"bucket.manage":             "Manage buckets",
	"credentials.metadata.read": "View credential metadata",
	"credentials.manage":        "Manage credentials",
	"connection.read":           "View connections",
	"connection.manage":         "Manage connections",
	"app.read":                  "View apps",
	"app.use":                   "Use apps",
	"app.create":                "Create apps",
	"app.manage":                "Manage apps",
	"app.tokens.manage":         "Manage app tokens",
	"catalogue.read":            "View the service catalogue",
	"catalogue.import":          "Import from the service catalogue",
	"catalogue.manage":          "Manage the service catalogue",
	"account.read":              "View account details",
	"account.manage":            "Manage account settings",
	"billing.read":              "View billing details",
	"billing.manage":            "Manage billing",
	"notification.update":       "Update notification settings",
	"audit.read":                "View audit history",
	"access.read":               "View access and permissions",
	"access.manage":             "Manage access and permissions",
}

func oauthScopeDescription(scope string) string {
	if description, ok := oauthScopeDescriptions[scope]; ok {
		return description
	}
	return scope
}

// renderOAuthErrorPage renders Engine-owned OAuth failures by reusing the
// same branded "connection failed" page the hosted-connect broker callback
// already shows (writeConnectCallbackFallback/connectCallbackTemplate),
// instead of maintaining a second near-duplicate template that can drift out
// of sync with the shared hosted-connect look and responsive layout.
func renderOAuthErrorPage(r *http.Request, s store.Store, w http.ResponseWriter, status int, message string) {
	writeConnectCallbackFallback(r.Context(), s, w, status, message, true)
}

// -- POST /oauth/token --

func oauthTokenHandler(service OAuthProviderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		if service == nil {
			writeOAuthTokenError(w, http.StatusServiceUnavailable, "server_error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
		if err := r.ParseForm(); err != nil {
			writeOAuthTokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		clientID, clientSecret := oauthClientCredentials(r)
		response, err := service.Token(r.Context(), oauthprovider.TokenRequest{
			GrantType: r.PostForm.Get("grant_type"), Code: r.PostForm.Get("code"),
			RedirectURI: r.PostForm.Get("redirect_uri"), CodeVerifier: r.PostForm.Get("code_verifier"),
			RefreshToken: r.PostForm.Get("refresh_token"), ClientID: clientID, ClientSecret: clientSecret,
		})
		if err != nil {
			writeOAuthTokenError(w, oauthTokenErrorStatus(err), oauthTokenErrorCode(err))
			return
		}
		writeOAuthJSON(w, http.StatusOK, map[string]any{
			"access_token": response.AccessToken, "refresh_token": response.RefreshToken,
			"token_type": response.TokenType, "expires_in": response.ExpiresIn,
			"scope": strings.Join(response.Scope, " "),
		})
	}
}

func oauthClientCredentials(r *http.Request) (string, string) {
	if id, secret, ok := r.BasicAuth(); ok {
		return id, secret
	}
	return r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
}

func oauthTokenErrorCode(err error) string {
	switch {
	case errors.Is(err, oauthprovider.ErrUnauthorizedClient):
		return "invalid_client"
	case errors.Is(err, oauthprovider.ErrInvalidGrant):
		return "invalid_grant"
	case errors.Is(err, oauthprovider.ErrUnsupportedGrantType):
		return "unsupported_grant_type"
	case errors.Is(err, oauthprovider.ErrInvalidRequest):
		return "invalid_request"
	default:
		return "server_error"
	}
}

func oauthTokenErrorStatus(err error) int {
	switch {
	case errors.Is(err, oauthprovider.ErrUnauthorizedClient):
		return http.StatusUnauthorized
	case errors.Is(err, oauthprovider.ErrInvalidGrant), errors.Is(err, oauthprovider.ErrUnsupportedGrantType), errors.Is(err, oauthprovider.ErrInvalidRequest):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func writeOAuthTokenError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// oauthRegisterRequest is the JSON body of POST /oauth/register. The redirect
// URI must be loopback and the scopes are the permission names the ephemeral
// client may later request at consent.
type oauthRegisterRequest struct {
	Name        string   `json:"name"`
	RedirectURI string   `json:"redirect_uri"`
	Scopes      []string `json:"scopes"`
}

// -- POST /oauth/register (dynamic client registration) --

// oauthRegisterHandler issues temporary credentials before user login and consent.
func oauthRegisterHandler(service OAuthProviderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		// Unconfigured Engines cannot issue credentials.
		if service == nil {
			writeOAuthTokenError(w, http.StatusServiceUnavailable, "server_error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
		var body oauthRegisterRequest
		// Reject malformed or extra input before any client is minted.
		if err := decodeOneStrictJSON(r.Body, &body); err != nil {
			writeOAuthTokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := service.RegisterClient(r.Context(), oauthprovider.RegisterClientRequest{
			RedirectURI: body.RedirectURI, Scopes: body.Scopes, Name: body.Name,
		})
		// Return protocol errors without leaking persistence details.
		if err != nil {
			writeOAuthTokenError(w, oauthTokenErrorStatus(err), oauthTokenErrorCode(err))
			return
		}
		writeOAuthJSON(w, http.StatusCreated, map[string]any{
			"client_id":            result.ClientID,
			"client_secret":        result.ClientSecret,
			"client_id_expires_at": result.ClientIDExpiresAt.Format(time.RFC3339),
		})
	}
}

// -- POST /oauth/revoke (RFC 7009) --

func oauthRevokeHandler(service OAuthProviderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		if service == nil {
			writeOAuthTokenError(w, http.StatusServiceUnavailable, "server_error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
		if err := r.ParseForm(); err != nil {
			writeOAuthTokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		clientID, clientSecret := oauthClientCredentials(r)
		err := service.Revoke(r.Context(), oauthprovider.RevokeRequest{
			Token: r.PostForm.Get("token"), ClientID: clientID, ClientSecret: clientSecret,
		})
		if errors.Is(err, oauthprovider.ErrUnauthorizedClient) {
			writeOAuthTokenError(w, http.StatusUnauthorized, "invalid_client")
			return
		}
		// RFC 7009: respond 200 whether or not a matching token existed, so this
		// endpoint cannot be used to probe token validity.
		w.WriteHeader(http.StatusOK)
	}
}

// -- Connected apps self-service (control-plane authenticated) --

func oauthConnectedAppsListHandler(service OAuthProviderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		if !ok {
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired, r.Context())
			return
		}
		apps, err := service.ListConnectedApps(r.Context(), actor)
		if err != nil {
			accesscontrol.WriteAuthorizationError(w, err, r.Context())
			return
		}
		writeOAuthJSON(w, http.StatusOK, map[string]any{"connected_apps": projectOAuthConnectedApps(apps)})
	}
}

func projectOAuthConnectedApps(apps []store.OAuthConnectedApp) []map[string]any {
	projected := make([]map[string]any, 0, len(apps))
	for _, app := range apps {
		projected = append(projected, map[string]any{
			"client_id": app.ClientID.String(), "client_name": app.ClientName,
			"scope": app.GrantedScope, "granted_at": app.GrantedAt.Format(time.RFC3339),
		})
	}
	return projected
}

func oauthConnectedAppsRevokeHandler(service OAuthProviderService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, ok := accesscontrol.ActorFromContext(r.Context())
		if !ok {
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired, r.Context())
			return
		}
		clientID, err := uuid.Parse(chi.URLParam(r, "client_id"))
		if err != nil {
			writeOAuthTokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if err := service.RevokeConnectedApp(r.Context(), actor, clientID); err != nil {
			if errors.Is(err, store.ErrOAuthConsentNotFound) {
				writeOAuthTokenError(w, http.StatusNotFound, "not_found")
				return
			}
			accesscontrol.WriteAuthorizationError(w, err, r.Context())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// -- RFC 8414 discovery metadata --

func oauthMetadataHandler(publicURL string) http.HandlerFunc {
	issuer := strings.TrimRight(publicURL, "/")
	return func(w http.ResponseWriter, r *http.Request) {
		writeOAuthJSON(w, http.StatusOK, map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/oauth/authorize",
			"token_endpoint":                        issuer + "/oauth/token",
			"revocation_endpoint":                   issuer + "/oauth/revoke",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"},
			"scopes_supported":                      accesscontrol.AllPermissions(),
		})
	}
}

func writeOAuthJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func setOAuthResponseHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

var _ OAuthProviderService = (*oauthprovider.Service)(nil)
