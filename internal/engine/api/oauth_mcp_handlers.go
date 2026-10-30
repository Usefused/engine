package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/browserauth"
	"github.com/Usefused/engine/internal/engine/oauthprovider"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type mcpOAuthService interface {
	RegisterMCPClient(context.Context, oauthprovider.MCPRegistration) (oauthprovider.CreateClientResult, error)
	AuthorizeMCP(context.Context, accesscontrol.Actor, oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error)
	ConsentMCP(context.Context, accesscontrol.Actor, oauthprovider.ConsentRequest, store.OAuthMCPGrant) (string, error)
	TokenMCP(context.Context, oauthprovider.TokenRequest, string) (oauthprovider.TokenResponse, error)
}

type mcpOAuthHandler struct {
	service   mcpOAuthService
	store     store.Store
	publicURL string
}

// MountMCPOAuthRoutes adds a runtime issuer while preserving existing management OAuth endpoints.
func MountMCPOAuthRoutes(router chi.Router, service OAuthProviderService, s store.Store, sessions BrowserSessionService, cookies *browserauth.CookieManager, publicURL string) {
	provider, ok := service.(mcpOAuthService)
	// Test fixtures and unconfigured installations must not advertise an unusable issuer.
	if !ok || strings.TrimSpace(publicURL) == "" {
		return
	}
	h := &mcpOAuthHandler{provider, s, strings.TrimRight(publicURL, "/")}
	limited := router.With(limitAuthenticationRequests(browserauth.NewRequestLimiter(30, 300, time.Minute)))
	limited.Get("/mcp/oauth/authorize", oauthAuthorizeHandler(h, s, sessions, cookies, "/login"))
	limited.Post("/mcp/oauth/consent", oauthConsentHandler(h, s, sessions, cookies, 128<<10))
	limited.Post("/mcp/oauth/token", oauthTokenHandler(h.exchangeToken))
	limited.Post("/mcp/oauth/register", oauthRegistrationHandler(16<<10, h.registerRequest))
	limited.Post("/mcp/oauth/revoke", oauthRevokeHandler(service))
	router.Get("/.well-known/oauth-authorization-server/mcp/oauth", oauthIssuerMetadataHandler(h.publicURL+"/mcp/oauth", h.publicURL+"/mcp/oauth", []string{store.OAuthMCPScope}, h.publicURL+"/mcp/oauth/register"))
	router.Get("/.well-known/oauth-protected-resource/mcp/{id}", h.resourceMetadata)
}

// resourceID accepts only this Engine's exact Streamable HTTP resources, never caller Host headers.
func (h *mcpOAuthHandler) resourceID(resource string) (uuid.UUID, error) {
	prefix := h.publicURL + "/mcp/"
	// Exact audience spelling prevents query, fragment, suffix, and cross-origin confusion.
	if !strings.HasPrefix(resource, prefix) {
		return uuid.Nil, errors.New("invalid resource")
	}
	id, err := uuid.Parse(strings.TrimPrefix(resource, prefix))
	// Canonical UUIDs ensure metadata and token audiences use the same URI.
	if err != nil || resource != prefix+id.String() {
		return uuid.Nil, errors.New("invalid resource")
	}
	return id, nil
}

// registerRequest adapts RFC 7591 metadata to the shared registration transport without granting server access.
func (h *mcpOAuthHandler) registerRequest(r *http.Request) (any, error) {
	var request oauthprovider.MCPRegistration
	// RFC 7591 extension fields may be ignored, but malformed JSON is never accepted.
	decoder := json.NewDecoder(r.Body)
	// Exactly one bounded JSON document may define a client registration.
	if decoder.Decode(&request) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return nil, errOAuthClientMetadata
	}
	result, err := h.service.RegisterMCPClient(r.Context(), request)
	// Unsafe callbacks or unsupported capabilities never yield a partial client identity.
	if err != nil {
		return nil, errOAuthClientMetadata
	}
	method := request.TokenEndpointAuthMethod
	// The RFC default is confidential client authentication with HTTP Basic.
	if method == "" {
		method = "client_secret_basic"
	}
	response := map[string]any{"client_id": result.Client.ClientID, "client_name": result.Client.Name, "redirect_uris": result.Client.RedirectURIs, "token_endpoint_auth_method": method, "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "scope": store.OAuthMCPScope, "client_id_issued_at": time.Now().Unix()}
	// Public clients never receive a fabricated secret field.
	if result.ClientSecret != "" {
		response["client_secret"] = result.ClientSecret
		response["client_secret_expires_at"] = 0
	}
	return response, nil
}

// resourceMetadata discloses only the public transport identity and its authorization server.
func (h *mcpOAuthHandler) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	// Malformed routes must not generate a plausible metadata document.
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeOAuthJSON(w, http.StatusOK, map[string]any{
		"resource": h.publicURL + "/mcp/" + id.String(), "authorization_servers": []string{h.publicURL + "/mcp/oauth"},
		"scopes_supported": []string{store.OAuthMCPScope}, "bearer_methods_supported": []string{"header"},
	})
}

// loadApp checks exact delegation rights before reading service or action metadata.
func (h *mcpOAuthHandler) loadApp(ctx context.Context, actor accesscontrol.Actor, resource string) (*store.AppCatalogItem, error) {
	id, err := h.resourceID(resource)
	// Another Engine's audience is never resolved locally.
	if err != nil {
		return nil, err
	}
	target, err := h.store.ResolveMCPRoute(ctx, id)
	// Unavailable or deactivated servers cannot receive new grants.
	if err != nil || target == nil {
		return nil, errors.New("MCP server unavailable")
	}
	ref := accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: target.AppFamilyID}
	err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor,
		accesscontrol.Requirement{Permission: accesscontrol.PermissionAppMCPUse, Resource: ref},
		accesscontrol.Requirement{Permission: accesscontrol.PermissionAppMCPTokensManage, Resource: ref})
	// Delegation requires the same authority as issuing a manual execution token.
	if err != nil {
		return nil, err
	}
	repository, ok := h.store.(store.AppCatalogRepository)
	// Never fall back to unauthenticated or Registry-based catalogue discovery.
	if !ok {
		return nil, errors.New("MCP catalogue unavailable")
	}
	item, err := repository.GetAuthorizedApp(ctx, actor.AccountID, target.AppID, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{target.AppFamilyID}})
	// This first runtime issuer is limited to Engine-hosted MCP applications.
	if err != nil || item == nil || item.Kind != store.AppKindMCP {
		return nil, errors.New("MCP server unavailable")
	}
	return item, nil
}

// authorizeRequest adds resource and runtime-scope policy before the common handler displays consent.
func (h *mcpOAuthHandler) authorizeRequest(r *http.Request, actor accesscontrol.Actor, req oauthprovider.AuthorizeRequest) (oauthprovider.AuthorizeResult, error) {
	// Resource indicators are singular; ambiguous audiences require a fresh request.
	if len(r.URL.Query()["resource"]) != 1 {
		return oauthprovider.AuthorizeResult{}, oauthprovider.ErrInvalidRequest
	}
	// An omitted scope requests runtime consent only, never management permissions.
	if len(req.Scope) == 0 {
		req.Scope = []string{store.OAuthMCPScope}
	}
	return h.service.AuthorizeMCP(r.Context(), actor, req)
}

// renderAuthorization exposes services and actions only after exact server delegation checks.
func (h *mcpOAuthHandler) renderAuthorization(w http.ResponseWriter, r *http.Request, actor accesscontrol.Actor, result oauthprovider.AuthorizeResult, req oauthprovider.AuthorizeRequest) {
	page, err := h.consentPage(r.Context(), actor, r.URL.Query().Get("resource"))
	// Missing rights or an incomplete catalogue must not display partial consent.
	if err != nil {
		renderOAuthErrorPage(r, h.store, w, http.StatusForbidden, "You need permission to use this MCP server and manage its tokens.")
		return
	}
	renderOAuthConsentPage(w, r, h.store, result, req, &page)
}

// consentRedirect revalidates the callback and the exact submitted actions behind shared browser/CSRF checks.
func (h *mcpOAuthHandler) consentRedirect(r *http.Request, actor accesscontrol.Actor, req oauthprovider.ConsentRequest) (string, error) {
	req.Scope = []string{store.OAuthMCPScope}
	_, err := h.service.AuthorizeMCP(r.Context(), actor, oauthprovider.AuthorizeRequest{ClientID: req.ClientID, RedirectURI: req.RedirectURI, ResponseType: "code", Scope: req.Scope, State: req.State, CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod})
	// Even denial must validate the registered callback before redirecting the browser.
	if err != nil {
		return "", err
	}
	// Cancellation creates no grant and preserves the original client state.
	if r.PostForm.Get("decision") == "deny" {
		return oauthDeniedRedirect(req)
	}
	// Unknown decisions must never be interpreted as approval.
	if r.PostForm.Get("decision") != "allow" {
		return "", oauthprovider.ErrInvalidRequest
	}
	grant, err := h.submittedGrant(r, actor)
	// Exact action membership and fixed account bindings remain mandatory before code issuance.
	if err != nil {
		return "", errOAuthSelection
	}
	return h.service.ConsentMCP(r.Context(), actor, req, grant)
}

// exchangeToken adds exact audience validation to the common token handler's parsed request.
func (h *mcpOAuthHandler) exchangeToken(r *http.Request, req oauthprovider.TokenRequest) (oauthprovider.TokenResponse, error) {
	// Repeated or absent resources cannot be treated as a multi-audience grant.
	if len(r.PostForm["resource"]) != 1 {
		return oauthprovider.TokenResponse{}, errOAuthTarget
	}
	resource := r.PostForm.Get("resource")
	// Only this Engine's canonical MCP endpoint may receive a runtime token.
	if _, err := h.resourceID(resource); err != nil {
		return oauthprovider.TokenResponse{}, errOAuthTarget
	}
	return h.service.TokenMCP(r.Context(), req, resource)
}

// oauthMCPResourceURL builds resource metadata URLs without trusting forwarded Host values.
func oauthMCPResourceURL(publicURL, path string) string {
	return strings.TrimRight(publicURL, "/") + "/.well-known/oauth-protected-resource" + path
}

// oauthMCPChallenge quotes the trusted metadata URI for RFC 9728 discovery.
func oauthMCPChallenge(publicURL, path string) string {
	return fmt.Sprintf("Bearer resource_metadata=%q, scope=%q", oauthMCPResourceURL(publicURL, path), store.OAuthMCPScope)
}

// canonicalMCPPath recognizes only the public Streamable HTTP endpoint, not internal bridges.
func canonicalMCPPath(path string) bool {
	id, err := uuid.Parse(strings.TrimPrefix(path, "/mcp/"))
	return err == nil && path == "/mcp/"+id.String()
}
