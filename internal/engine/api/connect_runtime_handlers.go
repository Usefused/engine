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
	"net"
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
	"go.opentelemetry.io/otel/trace"
)

const connectSessionTTL = 10 * time.Minute

type connectSessionStartRequest struct {
	EndUserRef     string            `json:"end_user_ref"`
	CreatedByAppID string            `json:"created_by_app_id,omitempty"`
	ReturnURL      string            `json:"return_url,omitempty"`
	ResourceInput  map[string]string `json:"resource_input,omitempty"`
	Scopes         []string          `json:"scopes,omitempty"`
}

type connectSessionStartResponse struct {
	AuthorizeURL      string    `json:"authorize_url"`
	ExpiresAt         time.Time `json:"expires_at"`
	Scopes            []string  `json:"-"`
	Route             string    `json:"-"`
	MissingFieldCount int       `json:"-"`
}

type oauthTokenResponse = connectauth.TokenResponse
type connectClientCredentials = connectauth.ClientCredentials

// StartConnectSessionHandler is the team/CLI-facing entry point; it creates a
// short-lived browser session without exposing OAuth client material.
func StartConnectSessionHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.session.start")
		defer span.End()
		// A failed default ensures every early validation/authorization return is
		// auditable; the bounded success projection overwrites it after persistence.
		span.SetAttributes(attribute.String("outcome", "failed"))

		call, ok := resolveConnectAdminCall(w, r, s)
		if !ok {
			return
		}
		req, createdByAppID, ok := decodeConnectSessionStartRequest(w, r)
		if !ok {
			return
		}
		resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey)
		if err != nil {
			writeConnectRuntimeError(w, err)
			return
		}
		response, err := createConnectSession(ctx, s, call, req.EndUserRef, createdByAppID, req.ReturnURL, req.ResourceInput, req.Scopes, resolved, masterKey)
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
		span.SetAttributes(connectSessionStartTelemetry(response)...)
		writeConnectJSON(w, http.StatusOK, response)
	}
}

// ConnectCallbackHandler owns the browser-return leg so provider tokens are
// exchanged and encrypted by Engine, never by generated SDK or CLI code.
func ConnectCallbackHandler(s store.Store, verifier ServiceVerifier, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.connect.callback")
		defer span.End()
		// Defaults make session, configuration, and provider failures observable
		// without copying raw callback or customer data into span attributes.
		span.SetAttributes(
			attribute.String("outcome", "failed"),
			attribute.String("resource_validation_status", "not_started"),
			attribute.Int("resource_discovered_count", 0),
			attribute.Int("resource_matched_count", 0),
			attribute.String("resource_persistence_status", "not_started"),
		)
		// Entry logging proves whether the browser returned from the provider while
		// retaining only bounded presence flags; code, state, errors, and URLs stay out.
		slog.InfoContext(ctx, "connect callback request received",
			"has_code", strings.TrimSpace(r.URL.Query().Get("code")) != "",
			"has_error", strings.TrimSpace(r.URL.Query().Get("error")) != "",
		)

		session, err := consumeConnectCallbackSession(ctx, s, r)
		if err != nil {
			writeConnectCallbackRuntimeError(ctx, s, w, err)
			return
		}
		call := connectAdminCall{bucketID: session.BucketID, serviceID: session.ServiceID}
		span.SetAttributes(connectAdminAttrs("connect.callback", call)...)
		// Legacy sessions without an unambiguous migration backfill fail closed;
		// the callback must never float to a newer provider contract.
		if session.ServiceVersionID == uuid.Nil {
			writeConnectCallbackFailure(ctx, s, w, r, session, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect session service version is unavailable"})
			return
		}
		resolved, err := resolveConnectRuntimeConfigForVersion(ctx, s, verifier, call, session.ServiceVersionID, masterKey)
		if err != nil {
			writeConnectCallbackFailure(ctx, s, w, r, session, err)
			return
		}
		if session.AuthType != resolved.config.AuthType || session.AuthName != resolved.config.AuthName {
			writeConnectCallbackFailure(ctx, s, w, r, session, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect auth configuration changed"})
			return
		}
		// Callback fallback scope metadata must describe what the user actually
		// consented to, not every scope the provider integration supports.
		token, err := exchangeConnectCallbackToken(ctx, r, session, resolved, masterKey)
		if err != nil {
			writeConnectCallbackFailure(ctx, s, w, r, session, err)
			return
		}
		plan, err := prepareCallbackResources(ctx, verifier, session, resolved.metadata, token)
		span.SetAttributes(callbackResourceValidationAttrs(plan)...)
		if err != nil {
			span.SetAttributes(attribute.String("resource_persistence_status", "not_started"))
			writeConnectCallbackFailure(ctx, s, w, r, session, connectRuntimeHTTPError{status: http.StatusBadGateway, message: err.Error()})
			return
		}
		conn, err := encryptAuthConnectionFromToken(session, resolved, token, masterKey)
		if err != nil {
			slog.ErrorContext(ctx, "failed to encrypt auth connection", slog.Any("error", err))
			span.SetAttributes(attribute.String("resource_persistence_status", "failed"))
			writeConnectCallbackFailure(ctx, s, w, r, session, connectRuntimeHTTPError{status: http.StatusInternalServerError, message: "failed to store auth connection"})
			return
		}
		saved, resourceCount, err := persistCallbackConnection(ctx, s, conn, plan)
		if err != nil {
			// Database errors may embed row values, so the control log records only
			// the fixed failing stage while the returned response remains generic.
			slog.ErrorContext(ctx, "failed to persist callback connection")
			span.SetAttributes(attribute.String("resource_persistence_status", "failed"))
			writeConnectCallbackFailure(ctx, s, w, r, session, connectRuntimeHTTPError{status: http.StatusInternalServerError, message: "failed to store auth connection"})
			return
		}
		span.SetAttributes(attribute.String("outcome", "success"), attribute.String("resource_persistence_status", "committed"), attribute.Int("resource_count", boundedCallbackResourceCount(resourceCount)))
		writeConnectCallbackSuccess(ctx, s, w, r, session, saved.ID)
	}
}

type connectRuntimeConfig struct {
	config      *store.ConnectConfig
	auth        fusedobject.AuthConfig
	flow        fusedobject.OAuth2FlowContract
	credentials connectClientCredentials
	metadata    *fusedobject.ServiceMetadata
}

type connectInputContractIdentity struct {
	ServiceVersionID string                            `json:"service_version_id"`
	AuthType         string                            `json:"auth_type"`
	AuthName         string                            `json:"auth_name"`
	Auth             fusedobject.AuthConfig            `json:"auth"`
	Flow             fusedobject.OAuth2FlowContract    `json:"flow"`
	Connect          *fusedobject.ServiceConnectConfig `json:"connect"`
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
	createdBy, err := optionalUUIDValue(req.CreatedByAppID)
	if err != nil {
		http.Error(w, "created_by_app_id must be a valid UUID", http.StatusBadRequest)
		return req, uuid.Nil, false
	}
	return req, createdBy, true
}

// resolveConnectRuntimeConfig ties a connect attempt to the bucket-enabled
// service version, which prevents onboarding against auth metadata the
// workspace will not use at runtime.
func resolveConnectRuntimeConfig(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, masterKey []byte) (connectRuntimeConfig, error) {
	return resolveConnectRuntimeConfigForVersion(ctx, s, verifier, call, uuid.Nil, masterKey)
}

// resolveConnectRuntimeConfigForVersion resolves either the latest start-time
// version or the exact immutable version pinned on a callback session.
func resolveConnectRuntimeConfigForVersion(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, serviceVersionID uuid.UUID, masterKey []byte) (connectRuntimeConfig, error) {
	cfg, err := s.GetConnectConfig(ctx, call.bucketID, call.serviceID)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("load connect config: %w", err)
	}
	if cfg == nil || !cfg.Enabled {
		return connectRuntimeConfig{}, connectRuntimeHTTPError{status: http.StatusNotFound, message: "connect config not found"}
	}
	metadata, err := loadConnectRuntimeMetadata(ctx, s, verifier, call, cfg.AuthType, serviceVersionID)
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	auth, flow, err := selectRuntimeOAuthConfig(metadata.AuthConfigs, cfg.AuthType, cfg.AuthName, connectOAuth2FlowName(metadata))
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	creds, err := decryptConnectClientCredentials(cfg, masterKey)
	if err != nil {
		return connectRuntimeConfig{}, fmt.Errorf("decrypt connect client credentials: %w", err)
	}
	return connectRuntimeConfig{config: cfg, auth: auth, flow: flow, credentials: creds, metadata: metadata}, nil
}

// loadConnectRuntimeMetadata loads and verifies one pinned service contract,
// then applies the effective workspace-owned connection profile snapshot.
func loadConnectRuntimeMetadata(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, authType string, serviceVersionID uuid.UUID) (*fusedobject.ServiceMetadata, error) {
	version, err := resolveConnectRuntimeVersion(ctx, s, call.serviceID, serviceVersionID)
	if err != nil {
		return nil, fmt.Errorf("load workspace service version: %w", err)
	}
	// Connect follows the workspace-pinned service metadata, not arbitrary
	// registry latest, so auth URLs/scopes match the version the Engine will
	// dispatch for this bucket.
	metadata, err := verifier.FetchServiceMetadata(ctx, call.serviceID, version)
	if err != nil {
		return nil, fmt.Errorf("load service metadata: %w", err)
	}
	// A Registry response must match the session's immutable identity; accepting
	// only its version label would permit metadata drift across the browser hop.
	if serviceVersionID != uuid.Nil && metadata.ServiceVersionID != serviceVersionID {
		return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect service version changed"}
	}
	metadata, err = attachedConnectMetadata(ctx, s, call, authType, metadata)
	if err != nil {
		return nil, err
	}
	return metadata, nil
}

// resolveConnectRuntimeVersion maps an exact local service-version ID back to
// its Registry version label, while preserving latest-version behavior at start.
func resolveConnectRuntimeVersion(ctx context.Context, s store.Store, serviceID, serviceVersionID uuid.UUID) (string, error) {
	if serviceVersionID == uuid.Nil {
		return s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, serviceID)
	}
	versionStore, ok := s.(store.WorkspaceServiceVersionLookupStore)
	// Exact callback resolution is an optional narrow capability so unrelated
	// Store test doubles do not acquire another method merely for Connect auth.
	if !ok {
		return "", errors.New("exact workspace service version lookup is unavailable")
	}
	version, err := versionStore.GetWorkspaceServiceVersion(ctx, serviceID, serviceVersionID)
	if err != nil {
		return "", err
	}
	return version.Version, nil
}

func connectOAuth2FlowName(metadata *fusedobject.ServiceMetadata) string {
	if metadata != nil && metadata.ConnectConfig != nil && strings.TrimSpace(metadata.ConnectConfig.OAuth2Flow) != "" {
		return metadata.ConnectConfig.OAuth2Flow
	}
	return "authorizationCode"
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

// createConnectSession validates caller-supplied routing data before deciding
// whether the next browser hop is the provider or Engine's collection page.
// OAuth state, nonce, and PKCE material are deliberately created only on the
// complete-input path so the form remains a pre-authorisation step.
func createConnectSession(ctx context.Context, s store.Store, call connectAdminCall, endUserRef string, createdByAppID uuid.UUID, returnURL string, resourceInput map[string]string, requestedScopes []string, resolved connectRuntimeConfig, masterKey []byte) (connectSessionStartResponse, error) {
	prepared, err := prepareConnectResourceInput(resolved.metadata.ConnectConfig, resourceInput)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	requestedScopes, err = applyAppConnectScopePolicy(ctx, s, call.bucketID, call.serviceID, createdByAppID, requestedScopes)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	effectiveScopes, err := resolveConnectScopes(resolved.auth, resolved.flow, requestedScopes)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	// Missing required fields are the only reason to launch Engine UI. Invalid
	// supplied values remain API errors so automation cannot silently change
	// from a fail-fast call into an interactive browser flow.
	if len(prepared.missing) != 0 {
		return createConnectInputSession(ctx, s, call, endUserRef, createdByAppID, returnURL, prepared.normalized, effectiveScopes, len(prepared.missing), resolved)
	}
	return createProviderConnectSession(ctx, s, call, endUserRef, createdByAppID, returnURL, prepared.canonical, effectiveScopes, resolved, masterKey)
}

type preparedConnectResourceInput struct {
	normalized map[string]string
	missing    []string
	canonical  []byte
}

// prepareConnectResourceInput separates omission detection from full resource
// derivation. Partial values are canonicalized for the one-time form, while a
// complete set also proves the final allowlisted provider base URL is valid.
func prepareConnectResourceInput(config *fusedobject.ServiceConnectConfig, values map[string]string) (preparedConnectResourceInput, error) {
	if config == nil || config.ResourceInput == nil {
		// A service without a declared input contract cannot safely assign meaning
		// to caller-provided customer fields, so non-empty input fails closed.
		if len(values) != 0 {
			return preparedConnectResourceInput{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "resource input is not supported"}
		}
		return preparedConnectResourceInput{normalized: map[string]string{}, canonical: []byte(`{}`)}, nil
	}
	normalized, missing, err := connectresource.NormalizeInput(config.ResourceInput, values)
	if err != nil {
		return preparedConnectResourceInput{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "resource input is invalid"}
	}
	if len(missing) != 0 {
		canonical, err := json.Marshal(normalized)
		return preparedConnectResourceInput{normalized: normalized, missing: missing, canonical: canonical}, err
	}
	resource, err := connectresource.FromInput(config.ResourceInput, normalized)
	if err != nil {
		return preparedConnectResourceInput{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "resource input is invalid"}
	}
	return preparedConnectResourceInput{normalized: normalized, canonical: resource.Metadata}, nil
}

// createConnectInputSession persists only a hashed one-time form token and
// non-secret request context. It does not allocate provider callback state,
// which keeps incomplete customer data outside the OAuth session lifecycle.
func createConnectInputSession(ctx context.Context, s store.Store, call connectAdminCall, endUserRef string, createdByAppID uuid.UUID, returnURL string, resourceInput map[string]string, scopes []string, missingFieldCount int, resolved connectRuntimeConfig) (connectSessionStartResponse, error) {
	token, err := randomURLToken(32)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	canonical, err := json.Marshal(resourceInput)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	contractHash, err := connectInputContractHash(resolved)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	expiresAt := time.Now().UTC().Add(connectSessionTTL)
	inputURL, err := buildConnectInputURL(resolved.credentials.RedirectURI, token)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	// Persistence is last so an invalid public callback origin cannot leave an
	// unreachable pending browser session behind.
	if _, err := s.CreateConnectInputSession(ctx, store.ConnectInputSession{
		BucketID: call.bucketID, ServiceID: call.serviceID,
		AuthType: resolved.config.AuthType, AuthName: resolved.config.AuthName, ContractHash: contractHash,
		EndUserRef: endUserRef, TokenHash: connectHash(token), CreatedByAppID: createdByAppID,
		ReturnURL: returnURL, ResourceInputJSON: canonical, RequestedScopes: scopes, ExpiresAt: expiresAt,
	}); err != nil {
		return connectSessionStartResponse{}, err
	}
	return connectSessionStartResponse{
		AuthorizeURL: inputURL, ExpiresAt: expiresAt, Scopes: scopes,
		Route: "hosted_form", MissingFieldCount: missingFieldCount,
	}, nil
}

// connectInputContractHash pins the service version, selected auth flow, and
// resource-input profile across the short browser handoff. The hash contains
// no OAuth client credential and is never exposed in telemetry or URLs.
func connectInputContractHash(resolved connectRuntimeConfig) (string, error) {
	identity := connectInputContractIdentity{
		ServiceVersionID: resolved.metadata.ServiceVersionID.String(),
		AuthType:         resolved.config.AuthType, AuthName: resolved.config.AuthName,
		Auth: resolved.auth, Flow: resolved.flow, Connect: resolved.metadata.ConnectConfig,
	}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// createProviderConnectSession creates the actual OAuth callback session only
// after resource input is complete. The helper is shared by direct API calls
// and successful hosted-form submissions to prevent two auth implementations.
func createProviderConnectSession(ctx context.Context, s store.Store, call connectAdminCall, endUserRef string, createdByAppID uuid.UUID, returnURL string, resourceInputJSON []byte, scopes []string, resolved connectRuntimeConfig, masterKey []byte) (connectSessionStartResponse, error) {
	session, response, err := buildProviderConnectSession(call, endUserRef, createdByAppID, returnURL, resourceInputJSON, scopes, resolved, masterKey)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	if _, err := s.CreateConnectSession(ctx, session); err != nil {
		return connectSessionStartResponse{}, err
	}
	return response, nil
}

// buildProviderConnectSession prepares a provider authorization request without
// persistence so hosted-form completion can atomically consume its one-time
// token and insert the resulting callback session in one store transaction.
func buildProviderConnectSession(call connectAdminCall, endUserRef string, createdByAppID uuid.UUID, returnURL string, resourceInputJSON []byte, scopes []string, resolved connectRuntimeConfig, masterKey []byte) (store.ConnectSession, connectSessionStartResponse, error) {
	state, verifier, nonce, err := newConnectSessionSecrets()
	if err != nil {
		return store.ConnectSession{}, connectSessionStartResponse{}, err
	}
	encrypted, err := encryptConnectPKCEVerifier(masterKey, verifier)
	if err != nil {
		return store.ConnectSession{}, connectSessionStartResponse{}, err
	}
	expiresAt := time.Now().UTC().Add(connectSessionTTL)
	authURL, err := buildConnectAuthorizeURL(resolved.auth, resolved.flow, scopes, resolved.credentials, state, pkceChallenge(verifier), nonce)
	if err != nil {
		return store.ConnectSession{}, connectSessionStartResponse{}, err
	}
	session := store.ConnectSession{
		BucketID:              call.bucketID,
		ServiceID:             call.serviceID,
		ServiceVersionID:      resolved.metadata.ServiceVersionID,
		AuthType:              resolved.config.AuthType,
		AuthName:              resolved.config.AuthName,
		EndUserRef:            endUserRef,
		StateHash:             connectHash(state),
		NonceHash:             connectHash(nonce),
		EncryptedDEK:          encrypted.wrappedDEK,
		EncryptedPKCEVerifier: encrypted.value,
		CreatedByAppID:        createdByAppID,
		ReturnURL:             returnURL,
		ResourceInputJSON:     resourceInputJSON,
		RequestedScopes:       scopes,
		ExpiresAt:             expiresAt,
	}
	return session, connectSessionStartResponse{AuthorizeURL: authURL, ExpiresAt: expiresAt, Scopes: scopes, Route: "direct"}, nil
}

// connectSessionStartTelemetry keeps REST, GraphQL, and gRPC start spans on one
// bounded attribute contract. Customer values, field names, URLs, scopes, and
// OAuth session material are intentionally excluded.
func connectSessionStartTelemetry(response connectSessionStartResponse) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("outcome", "success"),
		attribute.String("connect.input.route", response.Route),
		attribute.Int("connect.input.missing_field_count", response.MissingFieldCount),
		attribute.Int("scope_count", len(response.Scopes)),
	}
}

// applyAppConnectScopePolicy makes an app version's validated scope subset
// the consent ceiling whenever a connect session names that SDK/MCP runtime.
// Callers may narrow it further, but cannot silently expand it at runtime.
func applyAppConnectScopePolicy(ctx context.Context, s store.Store, bucketID, serviceID, appID uuid.UUID, requested []string) ([]string, error) {
	if appID == uuid.Nil {
		return requested, nil
	}
	scope, err := s.GetAppRuntime(ctx, appID)
	if errors.Is(err, store.ErrAppRuntimeNotFound) {
		return nil, connectRuntimeHTTPError{status: http.StatusForbidden, message: "app scope is unavailable"}
	}
	if err != nil || scope.BucketID != bucketID {
		return nil, connectRuntimeHTTPError{status: http.StatusForbidden, message: "app scope is unavailable"}
	}
	var selections []models.SDKSelection
	if err := json.Unmarshal(scope.Selections, &selections); err != nil {
		return nil, connectRuntimeHTTPError{status: http.StatusConflict, message: "app scope is invalid"}
	}
	policy := appScopesForService(selections, serviceID)
	if len(policy) == 0 {
		return requested, nil
	}
	if len(requested) == 0 {
		return append([]string(nil), policy...), nil
	}
	allowed := stringSet(policy)
	for _, value := range normalizedConnectScopes(requested) {
		if !allowed[value] {
			return nil, connectRuntimeHTTPError{status: http.StatusBadRequest, message: fmt.Sprintf("scope %q is outside the app policy", value)}
		}
	}
	return requested, nil
}

// appScopesForService reads the one immutable scope document already
// loaded for this request; matching by service prevents one selected
// provider's consent policy from affecting another provider in the app.
func appScopesForService(selections []models.SDKSelection, serviceID uuid.UUID) []string {
	for _, selection := range selections {
		if selection.ServiceID == serviceID {
			return selection.ConnectScopes
		}
	}
	return nil
}

// resolveConnectScopes lets callers reduce consent without allowing a runtime
// request to expand beyond the pinned service version's provider contract.
func resolveConnectScopes(auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, requested []string) ([]string, error) {
	approved := normalizedOAuth2FlowScopes(flow)
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

func normalizedOAuth2FlowScopes(flow fusedobject.OAuth2FlowContract) []string {
	values := make([]string, 0, len(flow.Scopes))
	for scope := range flow.Scopes {
		values = append(values, scope)
	}
	return normalizedConnectScopes(values)
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

type callbackResourcePlan struct {
	resources        []connectresource.Resource
	reconcile        bool
	validationStatus string
	discoveredCount  int
	matchedCount     int
}

// persistCallbackConnection uses the required transactional store method when
// resources are authoritative; profiles without resource routing retain the
// ordinary credential-only upsert.
func persistCallbackConnection(ctx context.Context, s store.Store, conn store.AuthConnection, plan callbackResourcePlan) (*store.AuthConnection, int, error) {
	if !plan.reconcile {
		// Profiles without authoritative resource discovery need only credential persistence.
		saved, err := s.UpsertAuthConnection(ctx, conn)
		return saved, 0, err
	}
	stored := projectConnectionResources(conn, plan.resources)
	saved, resources, err := s.UpsertAuthConnectionAndReconcileResources(ctx, conn, stored)
	return saved, len(resources), err
}

// storeConnectionResources centralizes the non-secret projection used by
// callback and manual discovery before authoritative reconciliation.
func storeConnectionResources(ctx context.Context, s store.Store, connection *store.AuthConnection, resources []connectresource.Resource) (int, error) {
	stored := projectConnectionResources(*connection, resources)
	for index := range stored {
		stored[index].ConnectionID = connection.ID
	}
	result, err := s.ReconcileConnectionResources(ctx, connection.ID, stored)
	return len(result), err
}

// projectConnectionResources converts provider discovery output to the
// credential-free routing rows accepted by both callback and manual stores.
func projectConnectionResources(connection store.AuthConnection, resources []connectresource.Resource) []store.ConnectionResource {
	stored := make([]store.ConnectionResource, 0, len(resources))
	for _, resource := range resources {
		stored = append(stored, store.ConnectionResource{
			BucketID: connection.BucketID, ServiceID: connection.ServiceID,
			ProviderResourceID: resource.ProviderID, ResourceType: resource.Type, DisplayName: resource.Name,
			BaseURL: resource.BaseURL, MetadataJSON: resource.Metadata, Scopes: resource.Scopes, IsActive: true,
		})
	}
	return stored
}

// prepareCallbackResources completes provider discovery and exact input
// matching before any new credential material reaches PostgreSQL.
func prepareCallbackResources(ctx context.Context, verifier ServiceVerifier, session *store.ConnectSession, metadata *fusedobject.ServiceMetadata, token oauthTokenResponse) (callbackResourcePlan, error) {
	plan := callbackResourcePlan{validationStatus: "not_required"}
	if metadata == nil || metadata.ConnectConfig == nil {
		return plan, nil
	}
	config := metadata.ConnectConfig
	if config.ResourceDiscovery != nil && (config.ResourceDiscovery.AutoRun == "" || config.ResourceDiscovery.AutoRun == "after_oauth_callback") {
		return prepareDiscoveredCallbackResources(ctx, verifier, session, metadata, token)
	}
	if config.ResourceInput == nil {
		return plan, nil
	}
	plan.reconcile = true
	values, err := connectSessionResourceInput(session)
	if err != nil {
		plan.validationStatus = "input_invalid"
		return plan, err
	}
	resource, err := connectresource.FromInput(config.ResourceInput, values)
	if err != nil {
		plan.validationStatus = "input_invalid"
		return plan, err
	}
	plan.resources = []connectresource.Resource{resource}
	plan.matchedCount = 1
	plan.validationStatus = "input_validated"
	return plan, nil
}

// prepareDiscoveredCallbackResources fetches the provider grant once and, for
// combined profiles, retains exactly the resource selected by validated input.
func prepareDiscoveredCallbackResources(ctx context.Context, verifier ServiceVerifier, session *store.ConnectSession, metadata *fusedobject.ServiceMetadata, token oauthTokenResponse) (callbackResourcePlan, error) {
	plan := callbackResourcePlan{reconcile: true, validationStatus: "discovery_failed"}
	discoveryVerifier, ok := verifier.(connectionDiscoveryVerifier)
	if !ok {
		return plan, errors.New("resource discovery is unavailable")
	}
	config := metadata.ConnectConfig
	endpoint, err := discoveryVerifier.FetchEndpointByName(ctx, session.ServiceID, metadata.ServiceVersionID.String(), config.ResourceDiscovery.OperationID)
	if err != nil {
		return plan, errors.New("resource discovery operation is unavailable")
	}
	resources, err := connectresource.Discover(ctx, metadata, endpoint, token.AccessToken, token.TokenType)
	if err != nil {
		return plan, err
	}
	plan.discoveredCount = len(resources)
	if config.ResourceInput == nil {
		plan.resources = resources
		plan.matchedCount = len(resources)
		plan.validationStatus = "discovery_validated"
		return plan, nil
	}
	values, err := connectSessionResourceInput(session)
	if err != nil {
		plan.validationStatus = "input_invalid"
		return plan, err
	}
	matched, err := connectresource.MatchDiscoveredInput(config.ResourceInput, values, resources)
	if err != nil {
		plan.validationStatus = callbackResourceMatchFailureStatus(err)
		return plan, err
	}
	plan.resources = []connectresource.Resource{matched}
	plan.matchedCount = 1
	plan.validationStatus = "match_validated"
	return plan, nil
}

// connectSessionResourceInput decodes the canonical map persisted by either
// direct start or hosted submission before callback validation.
func connectSessionResourceInput(session *store.ConnectSession) (map[string]string, error) {
	values, err := decodeConnectInputValues(session.ResourceInputJSON)
	// Callback responses keep stored JSON failures coarse while reusing the same
	// canonical string-map decoder as the hosted input path.
	if err != nil {
		return nil, errors.New("resource input is invalid")
	}
	return values, nil
}

// callbackResourceMatchFailureStatus maps matcher errors to a small telemetry
// enum without recording input, provider metadata, URLs, or raw error text.
func callbackResourceMatchFailureStatus(err error) string {
	if errors.Is(err, connectresource.ErrDiscoveryInputNoMatch) {
		return "match_not_found"
	}
	if errors.Is(err, connectresource.ErrDiscoveryInputAmbiguous) {
		return "match_ambiguous"
	}
	return "match_failed"
}

// callbackResourceValidationAttrs returns only bounded counts and a fixed
// status enum for callback auditing.
func callbackResourceValidationAttrs(plan callbackResourcePlan) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("resource_validation_status", plan.validationStatus),
		attribute.Int("resource_discovered_count", boundedCallbackResourceCount(plan.discoveredCount)),
		attribute.Int("resource_matched_count", boundedCallbackResourceCount(plan.matchedCount)),
	}
}

// boundedCallbackResourceCount caps telemetry cardinality while the complete
// authoritative set remains available to the transactional store.
func boundedCallbackResourceCount(count int) int {
	if count < 0 {
		return 0
	}
	if count > 1000 {
		return 1000
	}
	return count
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
// storage follows PKCE, offline-grant, and OIDC session checks.
func exchangeConnectCallbackToken(ctx context.Context, r *http.Request, session *store.ConnectSession, resolved connectRuntimeConfig, masterKey []byte) (oauthTokenResponse, error) {
	verifier, err := decryptConnectPKCEVerifier(session, masterKey)
	if err != nil {
		return oauthTokenResponse{}, fmt.Errorf("decrypt PKCE verifier: %w", err)
	}
	token, err := exchangeOAuthCode(ctx, http.DefaultClient, resolved.auth, resolved.flow, resolved.credentials, r.URL.Query().Get("code"), verifier)
	if err != nil {
		return oauthTokenResponse{}, connectRuntimeHTTPError{status: http.StatusBadGateway, message: "token exchange failed"}
	}
	// A provider-declared offline grant is part of the immutable auth contract;
	// accepting an access-only response would create a connection that fails later.
	if err := validateCallbackRefreshToken(ctx, resolved.auth, token); err != nil {
		return oauthTokenResponse{}, err
	}
	// OIDC nonce checking is the callback's proof that the id_token belongs to
	// the browser auth session we started, not just to any successful token
	// response from the provider.
	if err := validateOIDCNonce(resolved.auth, token.IDToken, session.NonceHash); err != nil {
		return oauthTokenResponse{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	return token, nil
}

// validateCallbackRefreshToken enforces provider-neutral offline grant policy
// before discovery, encryption, or the atomic connection/resource transaction.
func validateCallbackRefreshToken(ctx context.Context, auth fusedobject.AuthConfig, token oauthTokenResponse) error {
	status := "not_required"
	// Required grants must contain a usable value; whitespace cannot become a
	// stored credential or defer the failure to background refresh.
	if auth.RefreshTokenRequired {
		status = "satisfied"
		if strings.TrimSpace(token.RefreshToken) == "" {
			status = "missing"
			recordRefreshTokenRequirement(ctx, status)
			return connectRuntimeHTTPError{
				status:        http.StatusBadGateway,
				message:       "provider did not issue a required refresh token",
				publicMessage: "The provider did not grant offline access. Return to the application, start the connection again, and approve access when prompted.",
			}
		}
	}
	recordRefreshTokenRequirement(ctx, status)
	return nil
}

// recordRefreshTokenRequirement emits only a fixed decision class and never
// records token content, provider responses, customer identity, or URLs.
func recordRefreshTokenRequirement(ctx context.Context, status string) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("refresh_token_requirement_status", status))
}

// buildConnectAuthorizeURL preserves provider-supplied static query params
// while overriding security-sensitive OAuth fields from Engine-owned state.
func buildConnectAuthorizeURL(auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, scopes []string, creds connectClientCredentials, state, challenge, nonce string) (string, error) {
	if strings.TrimSpace(flow.AuthorizationURL) == "" {
		return "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "authorization_url is required"}
	}
	delimiter, err := scopeDelimiter(auth.ScopesDelimiter)
	if err != nil {
		return "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: err.Error()}
	}
	values := url.Values{}
	for key, value := range auth.ExtraAuthParams {
		values.Set(key, value)
	}
	values.Set("response_type", "code")
	values.Set("client_id", creds.ClientID)
	values.Set("redirect_uri", creds.RedirectURI)
	values.Set("state", state)
	values.Del("code_challenge")
	values.Del("code_challenge_method")
	// Providers that do not declare PKCE may reject its extension parameters,
	// so Engine strips provider extras and emits only the Engine-owned challenge
	// when the reviewed auth contract asks.
	if auth.PKCERequired {
		values.Set("code_challenge", challenge)
		values.Set("code_challenge_method", "S256")
	}
	values.Set("scope", strings.Join(scopes, delimiter))
	if isOIDCAuth(auth) {
		values.Set("nonce", nonce)
	}
	authURL, err := parseConnectAuthorizationURL(flow.AuthorizationURL)
	if err != nil {
		return "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "authorization_url must be absolute"}
	}
	authURL.RawQuery = mergeQuery(authURL.Query(), values).Encode()
	return authURL.String(), nil
}

// parseConnectAuthorizationURL admits secure provider endpoints and loopback
// HTTP used by local provider fixtures while rejecting active or ambiguous URLs.
func parseConnectAuthorizationURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Opaque != "" || parsed.User != nil || parsed.Hostname() == "" || strings.ContainsAny(parsed.Host, " \t\r\n;,\"'`") {
		return nil, errors.New("authorization URL is invalid")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	// Real provider authorization must use HTTPS; loopback HTTP remains available
	// for isolated tests without weakening non-local service contracts.
	if parsed.Scheme != "https" && !(parsed.Scheme == "http" && isConnectLoopbackHost(parsed.Hostname())) {
		return nil, errors.New("authorization URL transport is invalid")
	}
	return parsed, nil
}

// isConnectLoopbackHost recognizes browser-local OAuth fixtures without
// treating arbitrary HTTP hosts as trusted authorization providers.
func isConnectLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

// exchangeOAuthCode builds the standards-shaped code exchange in one place so
// refresh support can reuse the lower-level request/response handling later.
func exchangeOAuthCode(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract, creds connectClientCredentials, code, verifier string) (oauthTokenResponse, error) {
	return connectauth.ExchangeAuthorizationCode(ctx, client, auth, flow, creds, code, verifier)
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
	scopeSet := connectauth.TokenScopeMetadata(token, session.RequestedScopes, "request")
	return store.AuthConnection{
		BucketID:              session.BucketID,
		ServiceID:             session.ServiceID,
		ServiceVersionID:      session.ServiceVersionID,
		EndUserRef:            session.EndUserRef,
		CreatedByAppID:        session.CreatedByAppID,
		AuthType:              resolved.config.AuthType,
		AuthName:              resolved.config.AuthName,
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
func writeConnectCallbackSuccess(ctx context.Context, s store.Store, w http.ResponseWriter, r *http.Request, session *store.ConnectSession, connectionID uuid.UUID) {
	if session.ReturnURL == "" {
		// Without an application destination, the Engine owns the completion page.
		writeConnectCallbackFallback(ctx, s, w, http.StatusOK, "Connection complete. You can close this tab.", false)
		return
	}
	redirect, err := connectCallbackReturnURL(session.ReturnURL, map[string]string{
		"fused_connect": "success",
		"connection_id": connectionID.String(),
	})
	if err != nil {
		// A malformed stored destination cannot displace the safe local completion.
		writeConnectCallbackFallback(ctx, s, w, http.StatusOK, "Connection complete. You can close this tab.", false)
		return
	}
	http.Redirect(w, r, redirect, http.StatusFound)
}

// writeConnectCallbackFailure avoids JSON browser responses for completed
// sessions while keeping error details coarse and URL-safe.
func writeConnectCallbackFailure(ctx context.Context, s store.Store, w http.ResponseWriter, r *http.Request, session *store.ConnectSession, err error) {
	if session.ReturnURL == "" {
		// The Engine renders stable guidance when no application owns the result.
		writeConnectCallbackRuntimeError(ctx, s, w, err)
		return
	}
	redirect, redirectErr := connectCallbackReturnURL(session.ReturnURL, map[string]string{
		"fused_connect": "error",
		"error_code":    connectCallbackErrorCode(err),
	})
	if redirectErr != nil {
		// Invalid stored return metadata fails back to the hardened local page.
		writeConnectCallbackRuntimeError(ctx, s, w, err)
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

type connectCallbackPage struct {
	Branding hostedConnectBranding
	Message  string
	Failed   bool
}

var connectCallbackTemplate = parseHostedConnectTemplate("connect-callback", `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{if .Failed}}Connection failed{{else}}Connection complete{{end}} · {{.Branding.DisplayName}}</title>
{{template "hosted-connect-shell-style"}}
</head><body><main style="--connect-accent:{{.Branding.PrimaryColor}};--connect-accent-foreground:{{.Branding.AccentForeground}}"><header class="connect-brand">{{if .Branding.LogoURL}}<img class="connect-logo" src="{{.Branding.LogoURL}}" width="48" height="48" alt="" referrerpolicy="no-referrer">{{end}}<span>{{.Branding.DisplayName}}</span></header>
<p class="connect-eyebrow" data-tone="{{if .Failed}}danger{{else}}success{{end}}">{{if .Failed}}Connection not completed{{else}}Connected{{end}}</p>
<h1>{{if .Failed}}Connection failed{{else}}Connection complete{{end}}</h1><p class="connect-copy">{{.Message}}</p>
{{if or .Branding.SupportURL .Branding.PrivacyURL}}<div class="connect-links">{{if .Branding.SupportURL}}<a href="{{.Branding.SupportURL}}" rel="noreferrer">Support</a>{{end}}{{if .Branding.PrivacyURL}}<a href="{{.Branding.PrivacyURL}}" rel="noreferrer">Privacy</a>{{end}}</div>{{end}}
</main></body></html>`)

// writeConnectCallbackFallback renders the Engine-owned terminal browser page
// with validated branding and the same restrictive headers as hosted input.
func writeConnectCallbackFallback(ctx context.Context, s store.Store, w http.ResponseWriter, status int, message string, failed bool) {
	branding := loadHostedConnectBranding(ctx, s)
	page := connectCallbackPage{Branding: branding, Message: message, Failed: failed}
	writeHostedConnectHTML(w, status, "", branding, connectCallbackTemplate, page)
}

// writeConnectCallbackRuntimeError preserves the bounded status class while
// replacing internal/provider detail with stable browser-safe guidance.
func writeConnectCallbackRuntimeError(ctx context.Context, s store.Store, w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "Return to the application and start the connection again."
	var httpErr connectRuntimeHTTPError
	if errors.As(err, &httpErr) {
		// Only the preclassified HTTP status crosses into the browser response.
		status = httpErr.status
		// Stable public guidance may be shown when it contains no provider payload,
		// token material, customer values, or internal failure details.
		if httpErr.publicMessage != "" {
			message = httpErr.publicMessage
		}
	}
	writeConnectCallbackFallback(ctx, s, w, status, message, true)
}

// selectRuntimeOAuthConfig matches the bucket's chosen auth family instead of
// relying on registry order, which can differ between equivalent imports.
func selectRuntimeOAuthConfig(auths fusedobject.AuthConfigs, configuredType, configuredName, flowName string) (fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, error) {
	want := canonicalConnectAuthType(configuredType)
	for _, auth := range auths {
		if runtimeConnectAuthType(auth) == want && auth.Name == configuredName {
			flow, err := validateRuntimeOAuthConfig(auth, flowName)
			return auth, flow, err
		}
	}
	return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "service has no configured auth_name for " + want}
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
func validateRuntimeOAuthConfig(auth fusedobject.AuthConfig, flowName string) (fusedobject.OAuth2FlowContract, error) {
	if flowName != "authorizationCode" {
		return fusedobject.OAuth2FlowContract{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect requires the authorizationCode OAuth2 flow"}
	}
	flow, ok := auth.OAuth2Flows[flowName]
	if !ok || strings.TrimSpace(flow.TokenURL) == "" {
		return fusedobject.OAuth2FlowContract{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "selected OAuth2 flow requires token_url"}
	}
	if strings.TrimSpace(flow.AuthorizationURL) == "" {
		return fusedobject.OAuth2FlowContract{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "selected OAuth2 flow requires authorization_url"}
	}
	return flow, nil
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

// OAuth2 specs that declare openid in any canonical flow still require nonce
// validation even when their scheme type was not normalized to OpenID Connect.
func isOIDCAuth(auth fusedobject.AuthConfig) bool {
	if auth.Type == "openIdConnect" || strings.EqualFold(auth.Type, "oidc") {
		return true
	}
	for _, flow := range auth.OAuth2Flows {
		if _, ok := flow.Scopes["openid"]; ok {
			return true
		}
	}
	return false
}

// scopeDelimiter keeps provider quirks out of authorize URL construction.
func scopeDelimiter(value string) (string, error) {
	switch value {
	case "", "space":
		return " ", nil
	case "comma":
		return ",", nil
	default:
		return "", errors.New("scopes_delimiter must be space or comma")
	}
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
	status        int
	message       string
	publicMessage string
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
