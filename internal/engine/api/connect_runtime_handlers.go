package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/connectresource"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const connectSessionTTL = 10 * time.Minute

type connectSessionStartRequest struct {
	EndUserRef          string            `json:"end_user_ref"`
	CreatedByArtifactID string            `json:"created_by_artifact_id,omitempty"`
	ReturnURL           string            `json:"return_url,omitempty"`
	ResourceInput       map[string]string `json:"resource_input,omitempty"`
	Scopes              []string          `json:"scopes,omitempty"`
}

type connectSessionStartResponse struct {
	AuthorizeURL string    `json:"authorize_url"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scopes       []string  `json:"-"`
}

type oauthTokenResponse = connectauth.TokenResponse
type connectClientCredentials = connectauth.ClientCredentials

// StartConnectSessionHandler is the team/CLI-facing entry point; it creates a
// short-lived browser session without exposing OAuth client material.
func StartConnectSessionHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.session.start")
		defer span.End()

		call, ok := resolveConnectAdminCall(w, r, s)
		if !ok {
			return
		}
		req, createdByArtifactID, ok := decodeConnectSessionStartRequest(w, r)
		if !ok {
			return
		}
		resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey)
		if err != nil {
			writeConnectRuntimeError(w, err)
			return
		}
		response, err := createConnectSession(ctx, s, call, req.EndUserRef, createdByArtifactID, req.ReturnURL, req.ResourceInput, req.Scopes, resolved, masterKey)
		if err != nil {
			var requestErr connectRuntimeHTTPError
			if errors.As(err, &requestErr) {
				writeConnectRuntimeError(w, err)
				return
			}
			slog.ErrorContext(ctx, "failed to create connect session", slog.Any("error", err))
			http.Error(w, "failed to create connect session", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(connectAdminAttrs("connect.session.start", call)...)
		span.SetAttributes(attribute.Int("scope_count", len(response.Scopes)))
		writeConnectJSON(w, http.StatusOK, response)
	}
}

// ConnectCallbackHandler owns the browser-return leg so provider tokens are
// exchanged and encrypted by Engine, never by generated SDK or CLI code.
func ConnectCallbackHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.callback")
		defer span.End()

		session, err := consumeConnectCallbackSession(ctx, s, r)
		if err != nil {
			writeConnectRuntimeError(w, err)
			return
		}
		call := connectAdminCall{bucketID: session.BucketID, serviceID: session.ServiceID}
		resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey)
		if err != nil {
			writeConnectCallbackFailure(w, r, session, err)
			return
		}
		// Callback fallback scope metadata must describe what the user actually
		// consented to, not every scope the provider integration supports.
		resolved.auth.Scopes = append([]string(nil), session.RequestedScopes...)
		token, err := exchangeConnectCallbackToken(ctx, r, session, resolved, masterKey)
		if err != nil {
			writeConnectCallbackFailure(w, r, session, err)
			return
		}
		conn, err := encryptAuthConnectionFromToken(session, resolved, token, masterKey)
		if err != nil {
			slog.ErrorContext(ctx, "failed to encrypt auth connection", slog.Any("error", err))
			writeConnectCallbackFailure(w, r, session, connectRuntimeHTTPError{status: http.StatusInternalServerError, message: "failed to store auth connection"})
			return
		}
		saved, err := s.UpsertAuthConnection(ctx, conn)
		if err != nil {
			slog.ErrorContext(ctx, "failed to upsert auth connection", slog.Any("error", err))
			writeConnectCallbackFailure(w, r, session, connectRuntimeHTTPError{status: http.StatusInternalServerError, message: "failed to store auth connection"})
			return
		}
		resourceCount, err := reconcileCallbackResources(ctx, s, verifier, session, saved, resolved, token)
		if err != nil {
			span.SetAttributes(attribute.String("resource_discovery_status", "failed"))
			writeConnectCallbackFailure(w, r, session, connectRuntimeHTTPError{status: http.StatusBadGateway, message: err.Error()})
			return
		}
		span.SetAttributes(connectAdminAttrs("connect.callback", call)...)
		span.SetAttributes(attribute.String("resource_discovery_status", "ok"), attribute.Int("resource_count", resourceCount))
		writeConnectCallbackSuccess(w, r, session, saved.ID)
	}
}

type connectRuntimeConfig struct {
	config      *store.ConnectConfig
	auth        fusedobject.AuthConfig
	credentials connectClientCredentials
	metadata    *fusedobject.ServiceMetadata
}

// decodeConnectSessionStartRequest keeps request validation at the HTTP edge
// so downstream auth/session helpers only deal with normalized values.
func decodeConnectSessionStartRequest(w http.ResponseWriter, r *http.Request) (connectSessionStartRequest, uuid.UUID, bool) {
	var req connectSessionStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return req, uuid.Nil, false
	}
	req.EndUserRef = strings.TrimSpace(req.EndUserRef)
	req.ReturnURL = strings.TrimSpace(req.ReturnURL)
	if req.EndUserRef == "" {
		http.Error(w, "end_user_ref is required", http.StatusBadRequest)
		return req, uuid.Nil, false
	}
	if req.ReturnURL != "" && !isHTTPRedirectURI(req.ReturnURL) {
		http.Error(w, "return_url must be an absolute http or https URL", http.StatusBadRequest)
		return req, uuid.Nil, false
	}
	createdBy, err := optionalUUIDValue(req.CreatedByArtifactID)
	if err != nil {
		http.Error(w, "created_by_artifact_id must be a valid UUID", http.StatusBadRequest)
		return req, uuid.Nil, false
	}
	return req, createdBy, true
}

// resolveConnectRuntimeConfig ties a connect attempt to the bucket-enabled
// service version, which prevents onboarding against auth metadata the
// workspace will not use at runtime.
func resolveConnectRuntimeConfig(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, masterKey []byte) (connectRuntimeConfig, error) {
	cfg, err := s.GetConnectConfig(ctx, call.bucketID, call.serviceID)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("load connect config: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		return connectRuntimeConfig{}, connectRuntimeHTTPError{status: http.StatusNotFound, message: "connect config not found"}
	}
	version, err := s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, call.serviceID)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("load workspace service version: %w", err)
	}
	// Connect follows the workspace-pinned service metadata, not arbitrary
	// registry latest, so auth URLs/scopes match the version the Engine will
	// dispatch for this bucket.
	metadata, err := verifier.FetchServiceMetadata(ctx, call.serviceID, version)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("load service metadata: %w", err)
	}
	auth, err := selectRuntimeOAuthConfig(metadata.AuthConfigs, cfg.AuthType)
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	metadata, err = attachedConnectMetadata(ctx, s, call, cfg.AuthType, metadata)
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	creds, err := decryptConnectClientCredentials(cfg, masterKey)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("decrypt connect client credentials: %w", err)
	}
	return connectRuntimeConfig{config: cfg, auth: auth, credentials: creds, metadata: metadata}, nil
}

// attachedConnectMetadata overlays only the Engine-pinned effective profile
// snapshot (workspace override if present, else the pinned baseline); auth
// and operation metadata remain on the copied service value. Mutating a copy
// avoids contaminating a Registry-client cache shared by other buckets. The
// profile itself is workspace-scoped, not bucket-scoped -- every bucket in
// this workspace sees the same effective profile for this service version.
func attachedConnectMetadata(ctx context.Context, s store.Store, call connectAdminCall, authType string, metadata *fusedobject.ServiceMetadata) (*fusedobject.ServiceMetadata, error) {
	profileStore, ok := s.(store.WorkspaceProfileStore)
	if !ok || metadata == nil || metadata.ServiceVersionID == uuid.Nil {
		return metadata, nil
	}
	profile, err := profileStore.GetEffectiveWorkspaceProfile(ctx, call.serviceID, metadata.ServiceVersionID, canonicalConnectAuthType(authType))
	if err != nil || profile == nil {
		return metadata, err
	}
	var connectConfig fusedobject.ServiceConnectConfig
	if err := json.Unmarshal(profile.ProfileSnapshot, &connectConfig); err != nil {
		return nil, errors.New("attached connection profile snapshot is invalid")
	}
	copy := *metadata
	copy.ConnectConfig = &connectConfig
	return &copy, nil
}

// canonicalConnectAuthType collapses provider/importer spellings to the stable
// auth identity used by attachment and binding composite keys.
func canonicalConnectAuthType(authType string) string {
	switch strings.ToLower(strings.TrimSpace(authType)) {
	case "oauth", "oauth2":
		return "oauth"
	case "oidc", "openidconnect", "open_id_connect":
		return "oidc"
	default:
		return strings.ToLower(strings.TrimSpace(authType))
	}
}

// createConnectSession stores only hashed browser state and encrypted PKCE
// material so a stolen session row is not enough to complete token exchange.
func createConnectSession(ctx context.Context, s store.Store, call connectAdminCall, endUserRef string, createdByArtifactID uuid.UUID, returnURL string, resourceInput map[string]string, requestedScopes []string, resolved connectRuntimeConfig, masterKey []byte) (connectSessionStartResponse, error) {
	state, verifier, nonce, err := newConnectSessionSecrets()
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	encrypted, err := encryptConnectPKCEVerifier(masterKey, verifier)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	expiresAt := time.Now().UTC().Add(connectSessionTTL)
	resourceInputJSON, err := validateConnectResourceInput(resolved.metadata.ConnectConfig, resourceInput)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	requestedScopes, err = applyArtifactConnectScopePolicy(ctx, s, call.bucketID, call.serviceID, createdByArtifactID, requestedScopes)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	effectiveScopes, err := resolveConnectScopes(resolved.auth, requestedScopes)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	if _, err := s.CreateConnectSession(ctx, store.ConnectSession{
		BucketID:              call.bucketID,
		ServiceID:             call.serviceID,
		EndUserRef:            endUserRef,
		StateHash:             connectHash(state),
		NonceHash:             connectHash(nonce),
		EncryptedDEK:          encrypted.wrappedDEK,
		EncryptedPKCEVerifier: encrypted.value,
		CreatedByArtifactID:   createdByArtifactID,
		ReturnURL:             returnURL,
		ResourceInputJSON:     resourceInputJSON,
		RequestedScopes:       effectiveScopes,
		ExpiresAt:             expiresAt,
	}); err != nil {
		return connectSessionStartResponse{}, err
	}
	// Only the verifier is encrypted into the session; the browser receives the
	// derived challenge, which keeps the token exchange bound to this Engine.
	resolved.auth.Scopes = effectiveScopes
	authURL, err := buildConnectAuthorizeURL(resolved.auth, resolved.credentials, state, pkceChallenge(verifier), nonce)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	return connectSessionStartResponse{AuthorizeURL: authURL, ExpiresAt: expiresAt, Scopes: effectiveScopes}, nil
}

// applyArtifactConnectScopePolicy makes an artifact's validated scope subset
// the consent ceiling whenever a connect session names that SDK/MCP runtime.
// Callers may narrow it further, but cannot silently expand it at runtime.
func applyArtifactConnectScopePolicy(ctx context.Context, s store.Store, bucketID, serviceID, artifactID uuid.UUID, requested []string) ([]string, error) {
	if artifactID == uuid.Nil {
		return requested, nil
	}
	scope, err := s.GetArtifactScope(ctx, artifactID)
	if errors.Is(err, store.ErrArtifactScopeNotFound) {
		return nil, connectRuntimeHTTPError{status: http.StatusForbidden, message: "artifact scope is unavailable"}
	}
	if err != nil || scope.BucketID != bucketID {
		return nil, connectRuntimeHTTPError{status: http.StatusForbidden, message: "artifact scope is unavailable"}
	}
	var selections []models.SDKSelection
	if err := json.Unmarshal(scope.Selections, &selections); err != nil {
		return nil, connectRuntimeHTTPError{status: http.StatusConflict, message: "artifact scope is invalid"}
	}
	policy := artifactScopesForService(selections, serviceID)
	if len(policy) == 0 {
		return requested, nil
	}
	if len(requested) == 0 {
		return append([]string(nil), policy...), nil
	}
	allowed := stringSet(policy)
	for _, value := range normalizedConnectScopes(requested) {
		if !allowed[value] {
			return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("scope %q is outside the artifact policy", value)}
		}
	}
	return requested, nil
}

// artifactScopesForService reads the one immutable scope document already
// loaded for this request; matching by service prevents one selected
// provider's consent policy from affecting another provider in the artifact.
func artifactScopesForService(selections []models.SDKSelection, serviceID uuid.UUID) []string {
	for _, selection := range selections {
		if selection.ServiceID == serviceID {
			return selection.ConnectScopes
		}
	}
	return nil
}

// resolveConnectScopes lets callers reduce consent without allowing a runtime
// request to expand beyond the pinned service version's provider contract.
func resolveConnectScopes(auth fusedobject.AuthConfig, requested []string) ([]string, error) {
	approved := normalizedConnectScopes(auth.Scopes)
	if len(requested) == 0 {
		return approved, nil
	}
	selected := normalizedConnectScopes(requested)
	if len(selected) == 0 {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "scopes must contain at least one non-empty value"}
	}
	allowed := make(map[string]struct{}, len(approved))
	for _, scope := range approved {
		allowed[scope] = struct{}{}
	}
	for _, scope := range selected {
		if _, ok := allowed[scope]; !ok {
			return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("scope %q is not declared by this service", scope)}
		}
	}
	if isOIDCAuth(auth) && !connectContainsString(selected, "openid") {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "OIDC connect scopes must include openid"}
	}
	return selected, nil
}

// normalizedConnectScopes makes consent URLs and stored fallback metadata
// deterministic while removing duplicates that add no provider permission.
func normalizedConnectScopes(scopes []string) []string {
	seen := make(map[string]struct{}, len(scopes))
	normalized := make([]string, 0, len(scopes))
	for _, raw := range scopes {
		scope := strings.TrimSpace(raw)
		if scope == "" {
			continue
		}
		if _, exists := seen[scope]; exists {
			continue
		}
		seen[scope] = struct{}{}
		normalized = append(normalized, scope)
	}
	sort.Strings(normalized)
	return normalized
}

type connectionDiscoveryVerifier interface {
	FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error)
}

// reconcileCallbackResources runs only after token storage and only commits a
// complete discovery/input result, preserving old resources on provider errors.
func reconcileCallbackResources(ctx context.Context, s store.Store, verifier ServiceVerifier, session *store.ConnectSession, connection *store.AuthConnection, resolved connectRuntimeConfig, token oauthTokenResponse) (int, error) {
	config := resolved.metadata.ConnectConfig
	if config == nil {
		return 0, nil
	}
	resources, err := callbackResources(ctx, verifier, session, resolved.metadata, token)
	if err != nil {
		return 0, err
	}
	return storeConnectionResources(ctx, s, connection, resources)
}

// storeConnectionResources centralizes the non-secret projection used by
// callback and manual discovery before authoritative reconciliation.
func storeConnectionResources(ctx context.Context, s store.Store, connection *store.AuthConnection, resources []connectresource.Resource) (int, error) {
	stored := make([]store.ConnectionResource, 0, len(resources))
	for _, resource := range resources {
		stored = append(stored, store.ConnectionResource{
			ConnectionID: connection.ID, BucketID: connection.BucketID, ServiceID: connection.ServiceID,
			ProviderResourceID: resource.ProviderID, ResourceType: resource.Type, DisplayName: resource.Name,
			BaseURL: resource.BaseURL, MetadataJSON: resource.Metadata, Scopes: resource.Scopes, IsActive: true,
		})
	}
	result, err := s.ReconcileConnectionResources(ctx, connection.ID, stored)
	return len(result), err
}

// callbackResources chooses one declarative source; discovery takes precedence
// because it reflects the provider's post-consent access grant.
func callbackResources(ctx context.Context, verifier ServiceVerifier, session *store.ConnectSession, metadata *fusedobject.ServiceMetadata, token oauthTokenResponse) ([]connectresource.Resource, error) {
	config := metadata.ConnectConfig
	if config.ResourceDiscovery != nil && (config.ResourceDiscovery.AutoRun == "" || config.ResourceDiscovery.AutoRun == "after_oauth_callback") {
		discoveryVerifier, ok := verifier.(connectionDiscoveryVerifier)
		if !ok {
			return nil, errors.New("resource discovery is unavailable")
		}
		endpoint, err := discoveryVerifier.FetchEndpointByName(ctx, session.ServiceID, metadata.ServiceVersionID.String(), config.ResourceDiscovery.OperationID)
		if err != nil {
			return nil, errors.New("resource discovery operation is unavailable")
		}
		return connectresource.Discover(ctx, metadata, endpoint, token.AccessToken, token.TokenType)
	}
	if config.ResourceInput == nil {
		return nil, nil
	}
	values := map[string]string{}
	if err := json.Unmarshal(session.ResourceInputJSON, &values); err != nil {
		return nil, errors.New("resource input is invalid")
	}
	resource, err := connectresource.FromInput(config.ResourceInput, values)
	if err != nil {
		return nil, err
	}
	return []connectresource.Resource{resource}, nil
}

// validateConnectResourceInput binds only declared fields into the one-time
// session; undeclared values are discarded before they can reach metadata.
func validateConnectResourceInput(config *fusedobject.ServiceConnectConfig, values map[string]string) ([]byte, error) {
	if config == nil || config.ResourceInput == nil {
		return []byte(`{}`), nil
	}
	resource, err := connectresource.FromInput(config.ResourceInput, values)
	if err != nil {
		return nil, err
	}
	// FromInput's metadata contains only normalized declared fields and is safe
	// to bind to callback state; it never contains provider credentials.
	return resource.Metadata, nil
}

// consumeConnectCallbackSession enforces one-time callback use before token
// exchange, making replay protection independent of provider behavior.
func consumeConnectCallbackSession(ctx context.Context, s store.Store, r *http.Request) (*store.ConnectSession, error) {
	if providerErr := strings.TrimSpace(r.URL.Query().Get("error")); providerErr != "" {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "provider returned error: " + providerErr}
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" || strings.TrimSpace(r.URL.Query().Get("code")) == "" {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "state and code are required"}
	}
	session, err := s.GetConnectSessionByStateHash(ctx, connectHash(state))
	if err != nil {
		return nil, fmt.Errorf("load connect session: %w", err)
	}
	if session == nil || session.UsedAt != nil || time.Now().UTC().After(session.ExpiresAt) {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect session expired or already used"}
	}
	// Mark before token exchange so refresh/retry storms cannot replay the same
	// authorization code into multiple stored auth connections.
	if err := s.MarkConnectSessionUsed(ctx, session.StateHash, time.Now().UTC()); err != nil {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect session expired or already used"}
	}
	return session, nil
}

// exchangeConnectCallbackToken centralizes callback token validation so code
// storage only happens after PKCE and OIDC session checks have both passed.
func exchangeConnectCallbackToken(ctx context.Context, r *http.Request, session *store.ConnectSession, resolved connectRuntimeConfig, masterKey []byte) (oauthTokenResponse, error) {
	verifier, err := decryptConnectPKCEVerifier(session, masterKey)
	if err != nil {
		return oauthTokenResponse{}, fmt.Errorf("decrypt PKCE verifier: %w", err)
	}
	token, err := exchangeOAuthCode(ctx, http.DefaultClient, resolved.auth, resolved.credentials, r.URL.Query().Get("code"), verifier)
	if err != nil {
		return oauthTokenResponse{}, connectRuntimeHTTPError{status: http.StatusBadGateway, message: "token exchange failed"}
	}
	// OIDC nonce checking is the callback's proof that the id_token belongs to
	// the browser auth session we started, not just to any successful token
	// response from the provider.
	if err := validateOIDCNonce(resolved.auth, token.IDToken, session.NonceHash); err != nil {
		return oauthTokenResponse{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	return token, nil
}

// buildConnectAuthorizeURL preserves provider-supplied static query params
// while overriding security-sensitive OAuth fields from Engine-owned state.
func buildConnectAuthorizeURL(auth fusedobject.AuthConfig, creds connectClientCredentials, state, challenge, nonce string) (string, error) {
	if strings.TrimSpace(auth.AuthorizationURL) == "" {
		return "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "authorization_url is required"}
	}
	values := url.Values{}
	values.Set("response_type", "code")
	values.Set("client_id", creds.ClientID)
	values.Set("redirect_uri", creds.RedirectURI)
	values.Set("state", state)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	values.Set("scope", strings.Join(auth.Scopes, scopeDelimiter(auth.ScopesDelimiter)))
	if isOIDCAuth(auth) {
		values.Set("nonce", nonce)
	}
	for key, value := range auth.ExtraAuthParams {
		values.Set(key, value)
	}
	authURL, err := url.Parse(auth.AuthorizationURL)
	if err != nil || authURL.Scheme == "" || authURL.Host == "" {
		return "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "authorization_url must be absolute"}
	}
	authURL.RawQuery = mergeQuery(authURL.Query(), values).Encode()
	return authURL.String(), nil
}

// exchangeOAuthCode builds the standards-shaped code exchange in one place so
// refresh support can reuse the lower-level request/response handling later.
func exchangeOAuthCode(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds connectClientCredentials, code, verifier string) (oauthTokenResponse, error) {
	return connectauth.ExchangeAuthorizationCode(ctx, client, auth, creds, code, verifier)
}

// encryptAuthConnectionFromToken converts a provider token response into the
// bucket-owned connection record while preserving OIDC metadata for audits.
func encryptAuthConnectionFromToken(session *store.ConnectSession, resolved connectRuntimeConfig, token oauthTokenResponse, masterKey []byte) (store.AuthConnection, error) {
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return store.AuthConnection{}, err
	}
	access, err := store.EncryptWithDEK(dek, token.AccessToken)
	if err != nil {
		return store.AuthConnection{}, err
	}
	refresh, idToken, err := encryptOptionalTokens(dek, token)
	if err != nil {
		return store.AuthConnection{}, err
	}
	claims := connectauth.OIDCClaims(token.IDToken)
	scopeSet := connectauth.TokenScopeMetadata(token, resolved.auth.Scopes, "request")
	return store.AuthConnection{
		BucketID:              session.BucketID,
		ServiceID:             session.ServiceID,
		EndUserRef:            session.EndUserRef,
		CreatedByArtifactID:   session.CreatedByArtifactID,
		AuthType:              resolved.config.AuthType,
		EncryptedDEK:          wrappedDEK,
		EncryptedAccessToken:  access,
		EncryptedRefreshToken: refresh,
		EncryptedIDToken:      idToken,
		TokenType:             connectauth.DefaultTokenType(token.TokenType),
		Scopes:                scopeSet.Scopes,
		ScopeSource:           scopeSet.Source,
		Issuer:                connectauth.ClaimString(claims, "iss"),
		Subject:               connectauth.ClaimString(claims, "sub"),
		IdentityClaims:        connectauth.ClaimBytes(claims),
		ExpiresAt:             connectauth.TokenExpiresAt(token.ExpiresIn),
		RefreshTokenExpiresAt: connectauth.RefreshTokenExpiresAt(token.RefreshTokenExpiresIn),
		RefreshState:          "ok",
	}, nil
}

// writeConnectCallbackSuccess returns browsers to the stored return_url with
// only a connection handle; connection metadata is fetched server-side later.
func writeConnectCallbackSuccess(w http.ResponseWriter, r *http.Request, session *store.ConnectSession, connectionID uuid.UUID) {
	if session.ReturnURL == "" {
		writeConnectCallbackFallback(w, http.StatusOK, "Connection complete. You can close this tab.")
		return
	}
	redirect, err := connectCallbackReturnURL(session.ReturnURL, map[string]string{
		"fused_connect": "success",
		"connection_id": connectionID.String(),
	})
	if err != nil {
		writeConnectCallbackFallback(w, http.StatusOK, "Connection complete. You can close this tab.")
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// writeConnectCallbackFailure avoids JSON browser responses for completed
// sessions while keeping error details coarse and URL-safe.
func writeConnectCallbackFailure(w http.ResponseWriter, r *http.Request, session *store.ConnectSession, err error) {
	if session.ReturnURL == "" {
		writeConnectRuntimeError(w, err)
		return
	}
	redirect, redirectErr := connectCallbackReturnURL(session.ReturnURL, map[string]string{
		"fused_connect": "error",
		"error_code":    connectCallbackErrorCode(err),
	})
	if redirectErr != nil {
		writeConnectRuntimeError(w, err)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// connectCallbackReturnURL appends result markers to the prevalidated
// start-session return URL instead of trusting callback-time redirect input.
func connectCallbackReturnURL(raw string, params map[string]string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid return_url")
	}
	values := parsed.Query()
	for key, value := range params {
		values.Set(key, value)
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

// connectCallbackErrorCode maps internal/runtime failures to small codes that
// are safe to expose in a browser query string.
func connectCallbackErrorCode(err error) string {
	var httpErr connectRuntimeHTTPError
	if errors.As(err, &httpErr) && httpErr.message != "" {
		return strings.ReplaceAll(strings.ToLower(httpErr.message), " ", "_")
	}
	return "connect_runtime_failed"
}

// writeConnectCallbackFallback is the no-return-url browser fallback; it keeps
// the response human-readable without exposing connection JSON.
func writeConnectCallbackFallback(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<!doctype html><title>Fused Connect</title><p>%s</p>", message)
}

// selectRuntimeOAuthConfig matches the bucket's chosen auth family instead of
// relying on registry order, which can differ between equivalent imports.
func selectRuntimeOAuthConfig(auths fusedobject.AuthConfigs, configuredType string) (fusedobject.AuthConfig, error) {
	want := canonicalConnectAuthType(configuredType)
	for _, auth := range auths {
		if runtimeConnectAuthType(auth) == want {
			return auth, validateRuntimeOAuthConfig(auth)
		}
	}
	return fusedobject.AuthConfig{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "service has no configured " + want + " auth config"}
}

// runtimeConnectAuthType maps provider metadata into the small set of connect
// families exposed to bucket configuration.
func runtimeConnectAuthType(auth fusedobject.AuthConfig) string {
	if isOIDCAuth(auth) {
		return "oidc"
	}
	if isRuntimeOAuthAuth(auth) {
		return "oauth"
	}
	return ""
}

// validateRuntimeOAuthConfig catches incomplete registry metadata before a
// user is sent to a browser flow that cannot complete.
func validateRuntimeOAuthConfig(auth fusedobject.AuthConfig) error {
	if strings.TrimSpace(auth.TokenURL) == "" {
		return connectRuntimeHTTPError{status: http.StatusBadRequest, message: "token_url is required"}
	}
	if strings.TrimSpace(auth.AuthorizationURL) == "" {
		return connectRuntimeHTTPError{status: http.StatusBadRequest, message: "authorization_url is required"}
	}
	return nil
}

// decryptConnectClientCredentials unwraps apply-time OAuth app material only
// inside Engine, keeping service config files and SDKs on env refs/user refs.
func decryptConnectClientCredentials(cfg *store.ConnectConfig, masterKey []byte) (connectClientCredentials, error) {
	return connectauth.DecryptClientCredentials(cfg, masterKey)
}

type encryptedConnectValue struct {
	wrappedDEK string
	value      string
}

// encryptConnectPKCEVerifier keeps the verifier recoverable only by Engine so
// the browser can hold the challenge without becoming token-exchange capable.
func encryptConnectPKCEVerifier(masterKey []byte, verifier string) (encryptedConnectValue, error) {
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return encryptedConnectValue{}, err
	}
	encrypted, err := store.EncryptWithDEK(dek, verifier)
	if err != nil {
		return encryptedConnectValue{}, err
	}
	return encryptedConnectValue{wrappedDEK: wrappedDEK, value: encrypted}, nil
}

// decryptConnectPKCEVerifier makes callback exchange depend on the exact
// Engine session row created at start time, not on browser-supplied data.
func decryptConnectPKCEVerifier(session *store.ConnectSession, masterKey []byte) (string, error) {
	dek, err := store.UnwrapDEK(masterKey, session.EncryptedDEK)
	if err != nil {
		return "", err
	}
	return store.DecryptWithDEK(dek, session.EncryptedPKCEVerifier)
}

// encryptOptionalTokens stores refresh/id tokens with the same DEK as access
// tokens so future refresh can be added without changing the connection shape.
func encryptOptionalTokens(dek []byte, token oauthTokenResponse) (string, string, error) {
	refresh, err := encryptOptionalToken(dek, token.RefreshToken)
	if err != nil {
		return "", "", err
	}
	idToken, err := encryptOptionalToken(dek, token.IDToken)
	return refresh, idToken, err
}

// encryptOptionalToken keeps absent provider fields absent instead of
// inventing encrypted empty strings that look like usable token material.
func encryptOptionalToken(dek []byte, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}
	return store.EncryptWithDEK(dek, value)
}

// validateOIDCNonce treats OIDC identity as tied to the browser session, which
// blocks a swapped id_token from being stored as the connected user.
func validateOIDCNonce(auth fusedobject.AuthConfig, idToken, nonceHash string) error {
	if !isOIDCAuth(auth) {
		return nil
	}
	claims := connectauth.OIDCClaims(idToken)
	if len(claims) == 0 {
		return errors.New("id_token is required for OIDC")
	}
	nonce := connectauth.ClaimString(claims, "nonce")
	if nonce == "" {
		return errors.New("id_token nonce is required")
	}
	if connectHash(nonce) != nonceHash {
		return errors.New("id_token nonce mismatch")
	}
	return nil
}

// isRuntimeOAuthAuth intentionally accepts oidc aliases because registry
// imports may normalize OIDC providers differently across specs.
func isRuntimeOAuthAuth(auth fusedobject.AuthConfig) bool {
	return auth.Type == "oauth2" || auth.Type == "openIdConnect" || auth.Type == "oidc"
}

// isOIDCAuth also checks scopes so OAuth2 specs that request openid still get
// nonce validation even if their scheme type stayed oauth2.
func isOIDCAuth(auth fusedobject.AuthConfig) bool {
	return auth.Type == "openIdConnect" || strings.EqualFold(auth.Type, "oidc") || connectContainsString(auth.Scopes, "openid")
}

// scopeDelimiter keeps provider quirks out of authorize URL construction.
func scopeDelimiter(value string) string {
	if strings.EqualFold(value, "comma") {
		return ","
	}
	return " "
}

// mergeQuery lets provider-auth defaults exist while Engine-owned OAuth fields
// win when names overlap.
func mergeQuery(dst, src url.Values) url.Values {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	return dst
}

// newConnectSessionSecrets creates independent state, verifier, and nonce
// values so replay, PKCE, and OIDC checks do not share entropy.
func newConnectSessionSecrets() (string, string, string, error) {
	state, err := randomURLToken(32)
	if err != nil {
		return "", "", "", err
	}
	verifier, err := randomURLToken(32)
	if err != nil {
		return "", "", "", err
	}
	nonce, err := randomURLToken(32)
	return state, verifier, nonce, err
}

// randomURLToken emits URL-safe entropy because these values cross browser
// redirects and form bodies.
func randomURLToken(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// connectHash stores state/nonce lookup values as irreversible indexes rather
// than browser secrets.
func connectHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// pkceChallenge derives the browser-visible challenge while keeping the
// verifier encrypted server-side until callback.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// optionalUUIDValue allows CLI-first onboarding before an SDK exists while
// still validating audit attribution when a caller supplies it.
func optionalUUIDValue(raw string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(strings.TrimSpace(raw))
}

// connectContainsString keeps OIDC scope detection local to connect runtime
// instead of reusing broader slice helpers with different case semantics.
func connectContainsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type connectRuntimeHTTPError struct {
	status  int
	message string
}

// Error lets connect helpers preserve HTTP-safe failure messages without
// leaking lower-level token/client details to the browser.
func (e connectRuntimeHTTPError) Error() string { return e.message }

// writeConnectRuntimeError maps expected auth-flow failures to user-facing
// status codes while hiding internal encryption/store failures.
func writeConnectRuntimeError(w http.ResponseWriter, err error) {
	var httpErr connectRuntimeHTTPError
	if errors.As(err, &httpErr) {
		http.Error(w, httpErr.message, httpErr.status)
		return
	}
	http.Error(w, "connect runtime failed", http.StatusInternalServerError)
}
