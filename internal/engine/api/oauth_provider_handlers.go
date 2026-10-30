package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/ui/branding"
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
	var flow oauthBrowserFlow
	var exchange oauthTokenExchange
	// A missing provider keeps the shared handlers' explicit unavailable response.
	if service != nil {
		control := oauthControlFlow{service, s}
		flow, exchange = control, control.exchangeToken
	}
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Get("/oauth/authorize", oauthAuthorizeHandler(flow, s, sessions, cookies, loginPath))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Post("/oauth/authorize/consent", oauthConsentHandler(flow, s, sessions, cookies, maxOAuthFormBytes))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).
		Post("/oauth/token", oauthTokenHandler(exchange))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(60, 600, time.Minute))).
		Post("/oauth/revoke", oauthRevokeHandler(service))
	router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute))).
		Post("/oauth/register", oauthRegisterHandler(service))
	router.Get("/oauth/connected-apps", oauthConnectedAppsListHandler(service))
	router.Delete("/oauth/connected-apps/{client_id}", oauthConnectedAppsRevokeHandler(service))
	router.Get("/.well-known/oauth-authorization-server", oauthMetadataHandler(publicURL))
}

var (
	errOAuthTarget         = errors.New("invalid OAuth resource")
	errOAuthClientMetadata = errors.New("invalid OAuth client metadata")
	errOAuthUnavailable    = errors.New("OAuth provider unavailable")
	errOAuthSelection      = errors.New("invalid MCP consent selection")
)

// oauthBrowserFlow supplies only issuer policy and consent presentation; shared handlers own browser security and redirects.
type oauthBrowserFlow interface {
	authorizeRequest(*http.Request, accesscontrol.Actor, oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error)
	consentRedirect(*http.Request, accesscontrol.Actor, oauthprovider.ConsentRequest) (string, error)
	renderAuthorization(http.ResponseWriter, *http.Request, accesscontrol.Actor, oauthprovider.AuthorizeResult, oauthprovider.AuthorizeRequest)
}

type oauthControlFlow struct {
	service OAuthProviderService
	store   store.Store
}

// authorizeRequest preserves management scope checks and existing-consent behavior.
func (h oauthControlFlow) authorizeRequest(r *http.Request, actor accesscontrol.Actor, req oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error) {
	return h.service.Authorize(r.Context(), actor, req)
}

// consentRedirect preserves the management consent contract behind the common browser guard.
func (h oauthControlFlow) consentRedirect(r *http.Request, actor accesscontrol.Actor, req oauthprovider.ConsentRequest) (string, error) {
	// Only explicit approval can mint a credential.
	if r.PostForm.Get("decision") != "allow" {
		return oauthDeniedRedirect(req)
	}
	return h.service.Consent(r.Context(), actor, req)
}

// renderAuthorization keeps management permissions in their existing consent template.
func (h oauthControlFlow) renderAuthorization(w http.ResponseWriter, r *http.Request, _ accesscontrol.Actor, result oauthprovider.AuthorizeResult, req oauthprovider.AuthorizeRequest) {
	renderOAuthConsentPage(w, r, h.store, result, req, nil)
}

// exchangeToken delegates management tokens without attaching an MCP audience.
func (h oauthControlFlow) exchangeToken(r *http.Request, req oauthprovider.TokenRequest) (oauthprovider.TokenResponse, error) {
	return h.service.Token(r.Context(), req)
}

// oauthDeniedRedirect preserves client state while declining consent; issuer adapters own callback validation.
func oauthDeniedRedirect(req oauthprovider.ConsentRequest) (string, error) {
	target, ok := oauthErrorRedirect(req.RedirectURI, "access_denied", req.State)
	// An unusable callback must remain a local browser error.
	if !ok {
		return "", oauthprovider.ErrInvalidRequest
	}
	return target, nil
}

// -- GET /oauth/authorize --

// oauthAuthorizeHandler starts consent or reports a policy denial in the browser.
func oauthAuthorizeHandler(flow oauthBrowserFlow, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager, loginPath string) http.HandlerFunc {
	// The shared browser boundary applies before either issuer can inspect a request.
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		// Browser authorization cannot fall back to bearer credentials or an absent provider.
		if flow == nil || sessions == nil || cookies == nil {
			renderOAuthErrorPage(r, s, w, http.StatusServiceUnavailable, "OAuth is not available on this Engine.")
			return
		}
		actor, ok := resolveOAuthBrowserActor(r, sessions, cookies)
		// Preserve the original OAuth request across browser login.
		if !ok {
			http.Redirect(w, r, oauthLoginRedirectURL(loginPath, r), http.StatusFound)
			return
		}
		req := parseOAuthAuthorizeQuery(r)
		result, err := flow.authorizeRequest(r, actor, req)
		// Only protocol errors with an already-validated callback may leave this origin.
		if err != nil {
			// Invalid client identities and callbacks remain local errors.
			if target, redirectable := oauthRedirectableAuthorizeError(err, req); redirectable {
				http.Redirect(w, r, target, http.StatusFound)
				return
			}
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, oauthConsentFailureMessage(err))
			return
		}
		// Management consent may be reused; MCP policy always requests fresh action review.
		if !result.RequiresConsent {
			http.Redirect(w, r, result.RedirectURL, http.StatusFound)
			return
		}
		flow.renderAuthorization(w, r, actor, result, req)
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
func oauthConsentHandler(flow oauthBrowserFlow, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager, maxFormBytes int64) http.HandlerFunc {
	// Both consent forms use the same cookie and origin checks before processing grants.
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		// Browser authorization cannot fall back to bearer credentials or an absent provider.
		if flow == nil || sessions == nil || cookies == nil {
			renderOAuthErrorPage(r, s, w, http.StatusServiceUnavailable, "OAuth is not available on this Engine.")
			return
		}
		actor, ok := resolveOAuthBrowserActor(r, sessions, cookies)
		// An expired browser session cannot approve or deny a delegation.
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
		r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
		// Bound selection size before any issuer policy reads form fields.
		if err := r.ParseForm(); err != nil {
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, "The consent request was invalid.")
			return
		}
		req := oauthprovider.ConsentRequest{
			ClientID: r.PostForm.Get("client_id"), RedirectURI: r.PostForm.Get("redirect_uri"),
			Scope: splitOAuthScope(r.PostForm.Get("scope")), State: r.PostForm.Get("state"),
			CodeChallenge: r.PostForm.Get("code_challenge"), CodeChallengeMethod: r.PostForm.Get("code_challenge_method"),
		}
		redirectURL, err := flow.consentRedirect(r, actor, req)
		// Policy or persistence failures cannot produce a successful client callback.
		if err != nil {
			renderOAuthErrorPage(r, s, w, http.StatusBadRequest, oauthConsentFailureMessage(err))
			return
		}
		// End the native form navigation before the client callback so self-only form CSP does not block its redirect chain.
		writeConnectInputProviderPage(w, redirectURL, loadHostedConnectBranding(r.Context(), s))
	}
}

// oauthConsentFailureMessage gives users a way to free capacity without exposing internal errors.
func oauthConsentFailureMessage(err error) string {
	// A rejected runtime selection requires fresh action/account review rather than broader fallback access.
	if errors.Is(err, errOAuthSelection) {
		return "Choose at least one available action and supply its connected account when required. If the server changed, restart the connection."
	}
	// A full quota is actionable through the user's existing Connected Apps page.
	if errors.Is(err, store.ErrOAuthDynamicClientLimit) {
		return fmt.Sprintf("You already have %d active temporary OAuth clients. Disconnect one in Access → Connected Apps, or wait for it to expire, then try again.", store.MaxActiveOAuthDynamicClientsPerUser)
	}
	return "The connection could not be authorized. Return to the application and try again."
}

// -- Consent page rendering (server-rendered, mirrors connect_input_handlers.go) --

type oauthConsentPage struct {
	MCP                 *mcpOAuthConsentPage
	PickerScript        template.JS
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
  <style>`+branding.OAuthConsentStyles+`</style>
</head>
<body><main class="oauth-consent" style="--connect-accent:{{.Branding.PrimaryColor}};--connect-accent-foreground:{{.Branding.AccentForeground}}">
  <header class="connect-brand">{{if .Branding.LogoURL}}<img class="connect-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></header>
  {{/* Runtime consent names the server; management consent retains its permission summary. */}}
  {{if .MCP}}<p class="connect-eyebrow">Connect MCP server</p><h1>Allow {{.ClientName}} to use {{.MCP.Name}}?</h1>
  <p class="connect-copy">Choose the actions this connection can perform. New actions added later will need your approval.</p>
  {{else}}<p class="connect-eyebrow">Authorize application</p>
  <h1>{{.ClientName}} wants to access your account</h1>
  <p class="connect-copy">This application will be able to:</p>
  <ul class="oauth-scopes">{{range .ScopeLabels}}<li>{{.}}</li>{{end}}</ul>{{end}}
  <form method="post" action="{{.FormAction}}">
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="scope" value="{{.Scope}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
    {{/* Only MCP grants carry exact server identity and selectable service/action authority. */}}
    {{if .MCP}}
    <input type="hidden" name="resource" value="{{.MCP.Resource}}"><input type="hidden" name="app_id" value="{{.MCP.AppID}}">
    <section class="oauth-picker" aria-label="Service and operation permissions">
      <label class="oauth-search" hidden>Search services or operations<input type="search" placeholder="Search operations..." autocomplete="off"></label>
      <div class="oauth-services">
      {{/* Service disclosures keep the builder's card hierarchy and work without JavaScript. */}}
      {{range .MCP.Services}}{{if .Operations}}<details class="oauth-service" data-service="{{.Name}}">
        <summary><span><span class="oauth-service-name">{{.Name}}<span class="oauth-badge" hidden></span></span><span class="oauth-service-meta">{{len .Operations}} available operations</span></span></summary>
        <div class="oauth-service-body">
          {{/* Connected credentials stay explicitly bound to their original service input names. */}}
          {{if .Connected}}<div class="oauth-account"><p class="oauth-hint">Selected actions will use this connected account.</p>
          <label for="account_{{.Index}}">Connected account</label><select id="account_{{.Index}}" name="account_{{.Index}}"><option value="">Choose an account</option>{{range .Connections}}<option value="{{.Value}}">{{.Label}}</option>{{end}}</select>
          {{/* Missing connections must not imply that selecting an action creates account access. */}}
          {{if not .Connections}}<p class="oauth-hint">Connect an account for this service in Fused first, then restart this connection.</p>{{end}}</div>{{end}}
          <div class="oauth-operation-toolbar"><strong>Operations ({{len .Operations}})</strong><button type="button" class="oauth-select-all" hidden>Select shown</button></div>
          <div class="oauth-operation-list">
          {{range .Operations}}<label class="oauth-operation"><input type="checkbox" name="operation" value="{{.}}"><span>{{.}}</span></label>{{end}}
          </div>

        </div>
      </details>{{end}}{{end}}
      <p class="oauth-empty" role="status" hidden>No matching services or operations.</p>
      </div>
      <p class="oauth-selection-count" role="status" aria-live="polite" hidden></p>
    </section>
    <p class="oauth-hint">You can disconnect this application from Fused’s Connected apps page. Provider credentials stay in Fused.</p>
    {{end}}
    <div class="oauth-actions">
      <button class="oauth-deny" type="submit" name="decision" value="deny">Deny</button>
      <button class="oauth-allow" type="submit" name="decision" value="allow">{{if .MCP}}Allow selected actions{{else}}Allow{{end}}</button>
    </div>
  </form>
</main>
{{/* Only the MCP picker needs the fixed, CSP-hashed progressive enhancement. */}}
{{if .MCP}}<script>{{.PickerScript}}</script>{{end}}
</body></html>`)

// Pin the compiled asset bytes rather than allowing arbitrary inline consent-page scripts.
var oauthConsentScriptDigest = sha256.Sum256([]byte(branding.OAuthConsentScript))
var oauthConsentScriptHash = base64.StdEncoding.EncodeToString(oauthConsentScriptDigest[:])

// renderOAuthConsentPage shares branding, security headers, protocol fields, and the page shell across both consent policies.
func renderOAuthConsentPage(w http.ResponseWriter, r *http.Request, s store.Store, result oauthprovider.AuthorizeResult, req oauthprovider.AuthorizeRequest, mcp *mcpOAuthConsentPage) {
	pageBranding := loadHostedConnectBranding(r.Context(), s)
	labels := make([]string, 0, len(result.Scope))
	// Display only the protocol-validated scope labels.
	for _, scope := range result.Scope {
		labels = append(labels, oauthScopeDescription(scope))
	}
	page := oauthConsentPage{
		MCP: mcp, Branding: pageBranding, ClientName: result.Client.Name, ScopeLabels: labels,
		FormAction: "/oauth/authorize/consent", ClientID: req.ClientID, RedirectURI: req.RedirectURI,
		Scope: strings.Join(result.Scope, " "), State: req.State,
		CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod,
	}
	// MCP consent submits to its isolated issuer while sharing every browser-facing protocol field.
	if mcp != nil {
		page.FormAction = "/mcp/oauth/consent"
		// Only the compiled asset is trusted as JavaScript, preserving its exact CSP hash after template escaping.
		page.PickerScript = template.JS(branding.OAuthConsentScript)
	}
	writeConnectInputSecurityHeaders(w, "", pageBranding.LogoOrigin)
	// Admit only the compiled picker script; all other scripts and external connections remain blocked.
	if mcp != nil {
		w.Header().Set("Content-Security-Policy", w.Header().Get("Content-Security-Policy")+"; script-src 'sha256-"+oauthConsentScriptHash+"'")
	}
	// Native form POSTs need a real Origin for CSRF validation; cross-origin referrers still stay private.
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = oauthConsentTemplate.Execute(w, page)
}

var oauthScopeDescriptions = map[string]string{
	store.OAuthMCPScope:             "Connect to an MCP server with individually approved actions",
	"workspace.read":                "View workspace details",
	"workspace.update":              "Update workspace settings",
	"service.read":                  "View connected services",
	"service.consume":               "Use connected services",
	"service.manage":                "Manage connected services",
	"bucket.read":                   "View buckets",
	"bucket.values.read":            "Read bucket values",
	"bucket.use":                    "Use buckets",
	"bucket.manage":                 "Manage buckets",
	"credentials.reveal":            "Retrieve exportable secrets",
	"credentials.metadata.read":     "View credential metadata",
	"credentials.manage":            "Manage credentials",
	"connection.read":               "View connections",
	"connection.manage":             "Manage connections",
	"app.sdk.read":                  "View SDKs",
	"app.sdk.use":                   "Use SDKs",
	"app.sdk.create":                "Create SDKs",
	"app.sdk.manage":                "Manage SDKs",
	"app.sdk.tokens.manage":         "Manage execution tokens for SDKs",
	"app.unified_app.read":          "View Unified Apps",
	"app.unified_app.use":           "Use Unified Apps",
	"app.unified_app.create":        "Create Unified Apps",
	"app.unified_app.manage":        "Manage Unified Apps",
	"app.unified_app.tokens.manage": "Manage execution tokens for Unified Apps",
	"app.mcp.read":                  "View MCP servers",
	"app.mcp.use":                   "Use MCP servers",
	"app.mcp.create":                "Create MCP servers",
	"app.mcp.manage":                "Manage MCP servers",
	"app.mcp.tokens.manage":         "Manage execution tokens for MCP servers",
	"app.api.read":                  "View REST APIs",
	"app.api.use":                   "Use REST APIs",
	"app.api.create":                "Create REST APIs",
	"app.api.manage":                "Manage REST APIs",
	"app.api.tokens.manage":         "Manage execution tokens for REST APIs",
	"app.webhook.read":              "View webhooks",
	"app.webhook.create":            "Create webhooks",
	"app.webhook.manage":            "Manage webhooks",
	"catalogue.read":                "View the service catalogue",
	"catalogue.import":              "Import from the service catalogue",
	"catalogue.manage":              "Manage the service catalogue",
	"account.read":                  "View account details",
	"account.manage":                "Manage account settings",
	"billing.read":                  "View billing details",
	"billing.manage":                "Manage billing",
	"notification.update":           "Update notification settings",
	"audit.read":                    "View audit history",
	"access.read":                   "View access and permissions",
	"access.manage":                 "Manage access and permissions",
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

// oauthTokenExchange leaves issuer-specific audience checks behind the common form and client-credential parser.
type oauthTokenExchange func(*http.Request, oauthprovider.TokenRequest) (oauthprovider.TokenResponse, error)

// oauthTokenHandler owns bounded parsing, client credentials, protocol errors, and token responses for both issuers.
func oauthTokenHandler(exchange oauthTokenExchange) http.HandlerFunc {
	// No browser session is involved in this client-authenticated back-channel exchange.
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		// Unconfigured issuers cannot exchange credentials.
		if exchange == nil {
			writeOAuthTokenError(w, http.StatusServiceUnavailable, "server_error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxOAuthFormBytes)
		// Oversized or malformed forms cannot reach either issuer.
		if err := r.ParseForm(); err != nil {
			writeOAuthTokenError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		clientID, clientSecret := oauthClientCredentials(r)
		response, err := exchange(r, oauthprovider.TokenRequest{
			GrantType: r.PostForm.Get("grant_type"), Code: r.PostForm.Get("code"),
			RedirectURI: r.PostForm.Get("redirect_uri"), CodeVerifier: r.PostForm.Get("code_verifier"),
			RefreshToken: r.PostForm.Get("refresh_token"), ClientID: clientID, ClientSecret: clientSecret,
		})
		// Protocol errors must never echo private token or persistence details.
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

// oauthTokenErrorCode maps both issuer contracts to bounded, secret-safe protocol errors.
func oauthTokenErrorCode(err error) string {
	// Issuer-specific validation uses standard errors without exposing backend details.
	switch {
	// Audience and registration metadata are request errors shared by the MCP adapters.
	case errors.Is(err, errOAuthTarget):
		return "invalid_target"
	case errors.Is(err, errOAuthClientMetadata):
		return "invalid_client_metadata"
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

// oauthTokenErrorStatus distinguishes invalid requests from unavailable providers.
func oauthTokenErrorStatus(err error) int {
	// Unavailable configuration differs from client authentication and malformed grants.
	switch {
	case errors.Is(err, errOAuthUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, oauthprovider.ErrUnauthorizedClient):
		return http.StatusUnauthorized
	case errors.Is(err, errOAuthTarget), errors.Is(err, errOAuthClientMetadata), errors.Is(err, oauthprovider.ErrInvalidGrant), errors.Is(err, oauthprovider.ErrUnsupportedGrantType), errors.Is(err, oauthprovider.ErrInvalidRequest):
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
	// CLI registration keeps its loopback-only schema and response contract.
	return oauthRegistrationHandler(maxOAuthFormBytes, func(r *http.Request) (any, error) {
		// Unconfigured Engines cannot mint a client identity.
		if service == nil {
			return nil, errOAuthUnavailable
		}
		var body oauthRegisterRequest
		// CLI input remains strict; remote registration extensions do not relax this endpoint.
		if err := decodeOneStrictJSON(r.Body, &body); err != nil {
			return nil, oauthprovider.ErrInvalidRequest
		}
		result, err := service.RegisterClient(r.Context(), oauthprovider.RegisterClientRequest{RedirectURI: body.RedirectURI, Scopes: body.Scopes, Name: body.Name})
		// A failed registration must never expose a partial credential pair.
		if err != nil {
			return nil, err
		}
		return map[string]any{"client_id": result.ClientID, "client_secret": result.ClientSecret, "client_id_expires_at": result.ClientIDExpiresAt.Format(time.RFC3339)}, nil
	})
}

// oauthRegistrationHandler shares body bounds, secret-response headers, and error handling across registration dialects.
func oauthRegistrationHandler(maxBytes int64, register func(*http.Request) (any, error)) http.HandlerFunc {
	// Registration is public; no user or execution authority is granted by this handler.
	return func(w http.ResponseWriter, r *http.Request) {
		setOAuthResponseHeaders(w)
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		response, err := register(r)
		// Both dialects use the same bounded protocol error projection.
		if err != nil {
			writeOAuthTokenError(w, oauthTokenErrorStatus(err), oauthTokenErrorCode(err))
			return
		}
		writeOAuthJSON(w, http.StatusCreated, response)
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

// oauthMetadataHandler preserves the management issuer's existing discovery contract.
func oauthMetadataHandler(publicURL string) http.HandlerFunc {
	issuer := strings.TrimRight(publicURL, "/")
	return oauthIssuerMetadataHandler(issuer, issuer+"/oauth", accesscontrol.AllPermissions(), "")
}

// oauthIssuerMetadataHandler advertises the shared protocol implementation without mixing issuer scopes.
func oauthIssuerMetadataHandler(issuer, endpoints string, scopes any, registration string) http.HandlerFunc {
	metadata := map[string]any{
		"issuer": issuer, "authorization_endpoint": endpoints + "/authorize", "token_endpoint": endpoints + "/token",
		"revocation_endpoint": endpoints + "/revoke", "response_types_supported": []string{"code"},
		"grant_types_supported": []string{"authorization_code", "refresh_token"}, "code_challenge_methods_supported": []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post", "none"}, "scopes_supported": scopes,
	}
	// Only standards-based remote registration is advertised as RFC 7591.
	if registration != "" {
		metadata["registration_endpoint"] = registration
	}
	// Discovery contains only public configuration and uses the same JSON serializer as token responses.
	return func(w http.ResponseWriter, r *http.Request) { writeOAuthJSON(w, http.StatusOK, metadata) }
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
