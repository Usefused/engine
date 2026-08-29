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

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/authevent"
	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/connectresource"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const connectSessionTTL = 10 * time.Minute

type connectSessionStartRequest struct {
	EndUserRef     string            `json:"end_user_ref"`
	CreatedByAppID string            `json:"created_by_app_id,omitempty"`
	AuthType       string            `json:"auth_type,omitempty"`
	AuthName       string            `json:"auth_name,omitempty"`
	AuthRef        string            `json:"auth_ref,omitempty"`
	ReturnURL      string            `json:"return_url,omitempty"`
	ResourceInput  map[string]string `json:"resource_input,omitempty"`
	Scopes         []string          `json:"scopes,omitempty"`
}

type connectSessionStartResponse struct {
	AuthorizeURL      string    `json:"authorize_url"`
	ExpiresAt         time.Time `json:"expires_at"`
	ConnectSessionID  uuid.UUID `json:"-"`
	Scopes            []string  `json:"-"`
	Route             string    `json:"-"`
	MissingFieldCount int       `json:"-"`
}

type oauthTokenResponse = connectauth.TokenResponse
type connectClientCredentials = connectauth.ClientCredentials

// StartConnectSessionHandler is the team/CLI-facing entry point; it creates a
// short-lived browser session without exposing OAuth client material.
func StartConnectSessionHandler(s store.Store, verifier ServiceVerifier, masterKey []byte, redirectURIs ...string) http.HandlerFunc {
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
		req, createdByAppID, ok := decodeConnectSessionStartRequest(w, r, ctx)
		if !ok {
			return
		}
		// Optional SDK attribution is authorized independently and never participates in bucket credential selection.
		if err := validateConnectAuditSDK(ctx, s, createdByAppID); err != nil {
			writeConnectRuntimeError(w, ctx, err, "request_admission", "not_committed")
			return
		}
		// Control-plane app identity is audit attribution only; explicit auth_ref owns reusable credential routing.
		call.authType, call.authName, call.authRef = req.AuthType, req.AuthName, req.AuthRef
		resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey, firstRedirectURI(redirectURIs))
		// Resolution failures occur before any one-time connect session can be persisted.
		if err != nil {
			writeConnectRuntimeError(w, ctx, err, "connect_resolution", "not_committed")
			return
		}
		response, err := createConnectSession(ctx, s, call, req.EndUserRef, createdByAppID, req.ReturnURL, req.ResourceInput, req.Scopes, resolved, masterKey)
		// Session creation distinguishes reviewed admission errors from uncertain internal persistence failures.
		if err != nil {
			var requestErr connectRuntimeHTTPError
			// Reviewed validation failures occur before session persistence and retain precise client guidance.
			if errors.As(err, &requestErr) {
				writeConnectRuntimeError(w, ctx, err, "connect_session_admission", "not_committed")
				return
			}
			slog.ErrorContext(ctx, "failed to create connect session", slog.Any("error", err))
			// An unclassified persistence failure has an unknown commit outcome and must hide its internal cause.
			writeConnectRuntimeError(w, ctx, err, "connect_session_create", "unknown")
			return
		}
		span.SetAttributes(connectAdminAttrs("connect.session.start", call)...)
		span.SetAttributes(connectSessionStartTelemetry(response)...)
		writeConnectJSON(w, http.StatusOK, response)
	}
}

// validateConnectAuditSDK verifies one optional SDK Version ID against the control actor without loading credential routing.
func validateConnectAuditSDK(ctx context.Context, s store.Store, appID uuid.UUID) error {
	// Omitted attribution is valid for standalone consent and performs no app read.
	if appID == uuid.Nil {
		return nil
	}
	actor, ok := accesscontrol.ActorFromContext(ctx)
	// Attribution cannot be authorized without the control-plane actor already admitted by the route.
	if !ok {
		return connectRuntimeHTTPError{status: http.StatusUnauthorized, code: "connect_authentication_required", message: "authentication is required"}
	}
	familyID, err := resolveConnectAuditSDKFamily(ctx, s, actor.AccountID, appID)
	// Every unavailable identity shares one denial so the audit field cannot enumerate app versions.
	if err != nil {
		return connectAuditSDKUnavailableError()
	}
	requirement := accesscontrol.Requirement{Permission: accesscontrol.PermissionAppRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: familyID}}
	// Family-scoped app.read is the same visibility boundary used by SDK catalogue resolution.
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, requirement); err != nil {
		return connectAuditSDKUnavailableError()
	}
	return nil
}

// resolveConnectAuditSDKFamily resolves only SDK kind and ownership metadata needed for audit authorization.
func resolveConnectAuditSDKFamily(ctx context.Context, s store.Store, accountID, appID uuid.UUID) (uuid.UUID, error) {
	app, err := s.GetApp(ctx, appID)
	// Missing or cross-account versions cannot contribute a trusted family identity.
	if err != nil || app == nil || app.AccountID != accountID {
		return uuid.Nil, store.ErrAppNotFound
	}
	family, err := s.GetAppFamily(ctx, app.AppFamilyID)
	// MCP families and cross-account metadata are never valid for the CLI's SDK-only audit selector.
	if err != nil || family == nil || family.AccountID != accountID || family.Kind != store.AppKindSDK {
		return uuid.Nil, store.ErrAppFamilyNotFound
	}
	return family.AppFamilyID, nil
}

// connectAuditSDKUnavailableError keeps absent, wrong-kind, cross-account, and unauthorized attribution indistinguishable.
func connectAuditSDKUnavailableError() error {
	return connectRuntimeHTTPError{status: http.StatusForbidden, code: "connect_audit_sdk_unavailable", message: "SDK audit attribution is unavailable"}
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
		call := connectAdminCall{
			bucketID: session.BucketID, serviceID: session.ServiceID,
			authType: session.AuthType, authName: session.AuthName,
			credentialSource: persistedApplicationCredentialSource(
				session.CredentialSourceServiceID,
				session.CredentialSourceAuthType,
				session.CredentialSourceAuthName,
			),
		}
		span.SetAttributes(connectAdminAttrs("connect.callback", call)...)
		if err := validateConnectCallbackSession(session); err != nil {
			writeConnectCallbackFailure(ctx, s, w, r, session, err)
			return
		}
		resolved, err := resolveConnectRuntimeConfigForVersion(ctx, s, verifier, call, session.ServiceVersionID, masterKey, session.RedirectURI)
		if err != nil {
			writeConnectCallbackFailure(ctx, s, w, r, session, err)
			return
		}
		// The callback remains pinned to the exact scheme selected before the browser handoff.
		if session.AuthType != resolved.authType || session.AuthName != resolved.authName {
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
		// Notification delivery is best-effort after commit so a NATS outage cannot invalidate a saved provider grant.
		_ = authevent.Publish(ctx, authevent.NewConnectionCompleted(*session, *saved, resourceCount, time.Now().UTC()))
		writeConnectCallbackSuccess(ctx, s, w, r, session, saved.ID)
	}
}

// validateConnectCallbackSession rejects historical rows that cannot preserve the original provider handoff.
func validateConnectCallbackSession(session *store.ConnectSession) error {
	// Ambiguous version identity must never float to a newer provider contract.
	if session == nil || session.ServiceVersionID == uuid.Nil {
		return connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect session service version is unavailable"}
	}
	// Sessions created before callback pinning cannot reconstruct the redirect sent to the provider.
	if strings.TrimSpace(session.RedirectURI) == "" {
		return connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect session callback is unavailable; start a new connection"}
	}
	return nil
}

type connectRuntimeConfig struct {
	authType         string
	authName         string
	auth             fusedobject.AuthConfig
	flow             fusedobject.OAuth2FlowContract
	credentials      connectClientCredentials
	credentialSource connectauth.ApplicationCredentialSource
	metadata         *fusedobject.ServiceMetadata
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
func decodeConnectSessionStartRequest(w http.ResponseWriter, r *http.Request, ctx context.Context) (connectSessionStartRequest, uuid.UUID, bool) {
	var req connectSessionStartRequest
	// Malformed JSON is rejected before any connect configuration or session mutation is attempted.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_session_request", message: "invalid request body"}, "request_admission", "not_committed")
		return req, uuid.Nil, false
	}
	req.EndUserRef = strings.TrimSpace(req.EndUserRef)
	req.ReturnURL = strings.TrimSpace(req.ReturnURL)
	req.AuthType = canonicalConnectAuthType(req.AuthType)
	req.AuthName = strings.TrimSpace(req.AuthName)
	req.AuthRef = strings.TrimSpace(req.AuthRef)
	// An exact selector is all-or-none so a partial request cannot float across schemes.
	if (req.AuthType == "") != (req.AuthName == "") {
		writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_auth_selector", message: "auth_type and auth_name must be provided together"}, "request_admission", "not_committed")
		return req, uuid.Nil, false
	}
	// A malformed reference is rejected at admission before any bucket or provider metadata can be inferred from it.
	if req.AuthRef != "" {
		if _, err := parseAppAuthReference(req.AuthRef); err != nil {
			writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_auth_ref", message: "auth_ref must use ${bucket.auth.<service>.<authName>}"}, "request_admission", "not_committed")
			return req, uuid.Nil, false
		}
	}
	// Connection ownership requires a stable caller-supplied end-user reference.
	if req.EndUserRef == "" {
		writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "connect_end_user_ref_required", message: "end_user_ref is required"}, "request_admission", "not_committed")
		return req, uuid.Nil, false
	}
	// Return destinations must be absolute web URLs so callback routing cannot become ambiguous.
	if req.ReturnURL != "" && !isAbsoluteHTTPURL(req.ReturnURL) {
		writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_return_url", message: "return_url must be an absolute http or https URL"}, "request_admission", "not_committed")
		return req, uuid.Nil, false
	}
	createdBy, err := optionalUUIDValue(req.CreatedByAppID)
	// App attribution is optional, but a supplied identity must be an exact UUID.
	if err != nil {
		writeConnectRuntimeError(w, ctx, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_app_id", message: "created_by_app_id must be a valid UUID"}, "request_admission", "not_committed")
		return req, uuid.Nil, false
	}
	return req, createdBy, true
}

// isAbsoluteHTTPURL admits explicit browser return destinations without interpreting request host headers.
func isAbsoluteHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	// Relative and non-web destinations cannot be trusted as post-consent navigation targets.
	if err != nil {
		return false
	}
	return parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

// resolveConnectRuntimeConfig ties a connect attempt to the bucket-enabled
// service version, which prevents onboarding against auth metadata the
// workspace will not use at runtime.
func resolveConnectRuntimeConfig(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, masterKey []byte, redirectURI ...string) (connectRuntimeConfig, error) {
	return resolveConnectRuntimeConfigForVersion(ctx, s, verifier, call, uuid.Nil, masterKey, firstRedirectURI(redirectURI))
}

// resolveConnectRuntimeConfigForVersion resolves either the latest start-time
// version or the exact immutable version pinned on a callback session.
func resolveConnectRuntimeConfigForVersion(ctx context.Context, s store.Store, verifier ServiceVerifier, call connectAdminCall, serviceVersionID uuid.UUID, masterKey []byte, redirectURI ...string) (connectRuntimeConfig, error) {
	callbackURI := firstRedirectURI(redirectURI)
	// Consent cannot safely begin without the operator-controlled callback origin, but unrelated Engine work remains available.
	if callbackURI == "" {
		return connectRuntimeConfig{}, connectRuntimeHTTPError{
			status: http.StatusServiceUnavailable, code: "engine_public_url_required",
			message:     "OAuth connections require an Engine public URL.",
			remediation: "Configure engine.public_url or FUSED_ENGINE_PUBLIC_URL with the externally reachable Engine origin, then restart the Engine.",
		}
	}
	metadata, err := loadConnectRuntimeMetadata(ctx, s, verifier, call, call.authType, serviceVersionID)
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	authType, authName, err := resolveConnectAuthSelector(metadata.AuthConfigs, call.authType, call.authName)
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	// Profile resolution is keyed by the selected family, so auto-selection is followed by one exact overlay read.
	if call.authType == "" {
		metadata, err = attachedConnectMetadata(ctx, s, call, authType, metadata)
		if err != nil {
			return connectRuntimeConfig{}, err
		}
	}
	auth, flow, err := selectRuntimeOAuthConfig(metadata.AuthConfigs, authType, authName, connectOAuth2FlowName(metadata))
	if err != nil {
		return connectRuntimeConfig{}, err
	}
	call.authType, call.authName = authType, authName
	// Standalone control-plane consent resolves source identity from the explicit bucket reference, never app identity.
	if call.authRef != "" {
		source, err := resolveExplicitConnectCredentialSource(ctx, s, call)
		if err != nil {
			return connectRuntimeConfig{}, err
		}
		call.credentialSource = source
	}
	resolver := connectauth.NewApplicationCredentialResolver(s, masterKey, callbackURI)
	creds, err := resolver.Resolve(ctx, call.bucketID, call.serviceID, authType, authName, call.credentialSource)
	if err != nil {
		return connectRuntimeConfig{}, connectRuntimeHTTPError{status: http.StatusNotFound, message: "OAuth application credentials not found"}
	}
	return connectRuntimeConfig{authType: authType, authName: authName, auth: auth, flow: flow, credentials: creds, credentialSource: call.credentialSource, metadata: metadata}, nil
}

// resolveExplicitConnectCredentialSource admits one enabled source scheme through the planner's set-based local snapshot path.
func resolveExplicitConnectCredentialSource(ctx context.Context, s store.Store, call connectAdminCall) (connectauth.ApplicationCredentialSource, error) {
	parsed, err := parseAppAuthReference(call.authRef)
	// Invalid syntax must stop before any workspace identity can be resolved from its segments.
	if err != nil {
		return connectauth.ApplicationCredentialSource{}, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "invalid_connect_auth_ref", message: "auth_ref must use ${bucket.auth.<service>.<authName>}"}
	}
	contracts, err := connectGenerationContractStore(s)
	// Standalone reuse cannot bypass a missing local planning snapshot capability.
	if err != nil {
		return connectauth.ApplicationCredentialSource{}, err
	}
	sourceID, err := resolveConnectCredentialSourceID(ctx, contracts, parsed.ServiceKey)
	// Preserve the bounded identity-resolution error returned by the shared local lookup.
	if err != nil {
		return connectauth.ApplicationCredentialSource{}, err
	}
	version, err := resolveConnectCredentialSourceVersion(ctx, s, sourceID)
	// A source without one pinned workspace version cannot authorize credential reuse.
	if err != nil {
		return connectauth.ApplicationCredentialSource{}, err
	}
	// Contract compatibility must be proven before the source identity is persisted on consent state.
	if err := validateConnectCredentialSourceContract(ctx, contracts, sourceID, version, call.authType, parsed.AuthName); err != nil {
		return connectauth.ApplicationCredentialSource{}, err
	}
	// A self-reference obscures the direct family without providing reusable routing.
	if sourceID == call.serviceID && parsed.AuthName == call.authName {
		return connectauth.ApplicationCredentialSource{}, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "connect_auth_ref_self", message: "a credential cannot reference itself"}
	}
	return connectauth.ApplicationCredentialSource{ServiceID: sourceID, AuthType: call.authType, AuthName: parsed.AuthName}, nil
}

// connectGenerationContractStore exposes the exact local planning snapshot required for credential-source admission.
func connectGenerationContractStore(s store.Store) (store.GenerationContractStore, error) {
	contracts, ok := s.(store.GenerationContractStore)
	// Source routing must fail closed when the Engine cannot reuse the SDK/MCP planning boundary.
	if !ok {
		return nil, connectRuntimeHTTPError{status: http.StatusServiceUnavailable, code: "local_contract_store_unavailable", message: "local service contract storage is unavailable"}
	}
	return contracts, nil
}

// resolveConnectCredentialSourceID resolves one explicit service key without falling through to Registry discovery.
func resolveConnectCredentialSourceID(ctx context.Context, contracts store.GenerationContractStore, serviceKey string) (uuid.UUID, error) {
	resolved, err := contracts.ResolveGenerationServiceIDsByKeys(ctx, []string{serviceKey})
	// Transport or local-store failure is distinct from a syntactically valid but unavailable source.
	if err != nil {
		return uuid.Nil, connectRuntimeHTTPError{status: http.StatusConflict, code: "connect_auth_ref_unavailable", message: "referenced credential source is unavailable"}
	}
	sourceID, found := resolved[serviceKey]
	// Missing or ambiguous local identity cannot float to a similarly named Registry service.
	if !found || sourceID == uuid.Nil {
		return uuid.Nil, connectRuntimeHTTPError{status: http.StatusBadRequest, code: "connect_auth_ref_unavailable", message: "referenced credential source is not enabled in the workspace"}
	}
	return sourceID, nil
}

// resolveConnectCredentialSourceVersion pins standalone reuse to the source's one enabled workspace version.
func resolveConnectCredentialSourceVersion(ctx context.Context, s store.Store, sourceID uuid.UUID) (string, error) {
	services, err := s.ListAuthorizedWorkspaceServices(ctx, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{sourceID}}, nil)
	// Preserve an internal resolution failure rather than misreporting it as absent workspace membership.
	if err != nil {
		return "", connectRuntimeHTTPError{status: http.StatusInternalServerError, code: "connect_auth_ref_resolution_failed", message: "referenced credential source could not be resolved"}
	}
	// One exact source ID must retain one enabled workspace projection and pinned version.
	if len(services) != 1 || services[0].ServiceID != sourceID || strings.TrimSpace(services[0].Version) == "" {
		return "", connectRuntimeHTTPError{status: http.StatusConflict, code: "connect_auth_ref_unavailable", message: "referenced credential source has no enabled version"}
	}
	return services[0].Version, nil
}

// validateConnectCredentialSourceContract admits the exact referenced scheme from the pinned source snapshot.
func validateConnectCredentialSourceContract(ctx context.Context, contracts store.GenerationContractStore, sourceID uuid.UUID, version, targetAuthType, sourceAuthName string) error {
	authContracts, err := contracts.ListGenerationAuthContracts(ctx, []store.GenerationAuthSelection{{ServiceID: sourceID, Version: version}}, false)
	// An unavailable immutable contract cannot be replaced by current Registry metadata.
	if err != nil {
		return connectRuntimeHTTPError{status: http.StatusConflict, code: "connect_auth_ref_unavailable", message: "referenced credential source contract is unavailable"}
	}
	// Exact family and scheme admission prevents a later sibling auth definition from changing the credential route.
	if len(authContracts) != 1 || !appAuthContractContainsSource(authContracts[0].AuthConfigs, targetAuthType, sourceAuthName) {
		return connectRuntimeHTTPError{status: http.StatusBadRequest, code: "connect_auth_ref_incompatible", message: "referenced credential source does not declare a compatible auth scheme"}
	}
	return nil
}

// connectCallFromAppSelection applies one validated selection without another store read.
func connectCallFromAppSelection(call connectAdminCall, selection models.SDKSelection) (connectAdminCall, error) {
	authType := canonicalConnectAuthType(selection.AuthType)
	authName := strings.TrimSpace(selection.AuthName)
	// Connect requires one exact OAuth/OIDC scheme even when ordinary operation dispatch accepts alternatives.
	if (authType != "oauth" && authType != "oidc") || authName == "" {
		return connectAdminCall{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "app selection does not declare an OAuth/OIDC auth scheme"}
	}
	// Caller selectors may narrow direct consent, but cannot override the immutable app selection.
	if (call.authType != "" || call.authName != "") && (canonicalConnectAuthType(call.authType) != authType || strings.TrimSpace(call.authName) != authName) {
		return connectAdminCall{}, connectRuntimeHTTPError{status: http.StatusBadRequest, message: "connect auth selector does not match the app selection"}
	}
	source, err := applicationCredentialSourceForSelection(selection)
	// Partial or cross-family persisted source identity indicates an invalid immutable scope.
	if err != nil {
		return connectAdminCall{}, connectRuntimeHTTPError{status: http.StatusConflict, message: "app credential source is invalid"}
	}
	call.authType, call.authName = authType, authName
	call.credentialSource = source
	call.appConnectScopes = append([]string(nil), selection.ConnectScopes...)
	return call, nil
}

// applicationCredentialSourceForSelection returns direct target semantics unless a complete app reference was planned.
func applicationCredentialSourceForSelection(selection models.SDKSelection) (connectauth.ApplicationCredentialSource, error) {
	target := connectauth.ApplicationCredentialSource{
		ServiceID: selection.ServiceID,
		AuthType:  canonicalConnectAuthType(selection.AuthType),
		AuthName:  strings.TrimSpace(selection.AuthName),
	}
	hasService := selection.CredentialSourceServiceID != uuid.Nil
	hasType := strings.TrimSpace(selection.CredentialSourceAuthType) != ""
	hasName := strings.TrimSpace(selection.CredentialSourceAuthName) != ""
	// An absent reference deliberately resolves to the target service's own registration.
	if !hasService && !hasType && !hasName {
		return target, nil
	}
	// Persisted source identity is atomic so callback and refresh never guess missing pieces.
	if !hasService || !hasType || !hasName {
		return connectauth.ApplicationCredentialSource{}, errors.New("credential source identity is incomplete")
	}
	source := connectauth.ApplicationCredentialSource{
		ServiceID: selection.CredentialSourceServiceID,
		AuthType:  canonicalConnectAuthType(selection.CredentialSourceAuthType),
		AuthName:  strings.TrimSpace(selection.CredentialSourceAuthName),
	}
	// References may change service and scheme name, but never the OAuth/OIDC family selected for the target.
	if source.AuthType != target.AuthType || source.AuthType == "" {
		return connectauth.ApplicationCredentialSource{}, errors.New("credential source auth family does not match target")
	}
	return source, nil
}

// persistedApplicationCredentialSource reconstructs the immutable routing identity carried by browser sessions and grants.
func persistedApplicationCredentialSource(serviceID uuid.UUID, authType, authName string) connectauth.ApplicationCredentialSource {
	return connectauth.ApplicationCredentialSource{
		ServiceID: serviceID,
		AuthType:  canonicalConnectAuthType(authType),
		AuthName:  strings.TrimSpace(authName),
	}
}

// effectiveApplicationCredentialSource fills direct callers with target semantics before persistence.
func effectiveApplicationCredentialSource(call connectAdminCall, resolved connectRuntimeConfig) connectauth.ApplicationCredentialSource {
	// App-scoped calls already carry a complete source selected at plan time.
	if call.credentialSource.ServiceID != uuid.Nil {
		return call.credentialSource
	}
	// Standalone auth_ref resolution is returned with runtime config because call is passed by value during admission.
	if resolved.credentialSource.ServiceID != uuid.Nil {
		return resolved.credentialSource
	}
	return connectauth.ApplicationCredentialSource{ServiceID: call.serviceID, AuthType: resolved.authType, AuthName: resolved.authName}
}

// resolveConnectAuthSelector requires an exact OAuth/OIDC scheme or an unambiguous sole candidate.
func resolveConnectAuthSelector(auths fusedobject.AuthConfigs, requestedType, requestedName string) (string, string, error) {
	requestedType = canonicalConnectAuthType(requestedType)
	requestedName = strings.TrimSpace(requestedName)
	// Explicit selectors preserve the immutable app/CLI decision without relying on metadata order.
	if requestedType != "" || requestedName != "" {
		if requestedType == "" || requestedName == "" {
			return "", "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "auth_type and auth_name must be provided together"}
		}
		return requestedType, requestedName, nil
	}
	var selected *fusedobject.AuthConfig
	for index := range auths {
		// Only browser-connect OAuth/OIDC definitions participate in implicit selection.
		if runtimeConnectAuthType(auths[index]) == "" {
			continue
		}
		if selected != nil {
			return "", "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "service has multiple OAuth/OIDC schemes; provide auth_type and auth_name"}
		}
		selected = &auths[index]
	}
	// A service without a browser-connect scheme cannot initiate consent.
	if selected == nil || strings.TrimSpace(selected.Name) == "" {
		return "", "", connectRuntimeHTTPError{status: http.StatusBadRequest, message: "service has no OAuth/OIDC authorization scheme"}
	}
	return runtimeConnectAuthType(*selected), strings.TrimSpace(selected.Name), nil
}

// firstRedirectURI returns the injected canonical callback without consulting request headers.
func firstRedirectURI(values []string) string {
	// Production injects one validated URI; empty remains a closed test/setup failure at provider start.
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
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
	requestedScopes, err = applyAppConnectScopePolicy(call.appConnectScopes, requestedScopes)
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
	source := effectiveApplicationCredentialSource(call, resolved)
	expiresAt := time.Now().UTC().Add(connectSessionTTL)
	inputURL, err := buildConnectInputURL(resolved.credentials.RedirectURI, token)
	if err != nil {
		return connectSessionStartResponse{}, err
	}
	// Persistence is last so an invalid public callback origin cannot leave an
	// unreachable pending browser session behind.
	correlationID := uuid.New()
	if _, err := s.CreateConnectInputSession(ctx, store.ConnectInputSession{
		ID:       correlationID,
		BucketID: call.bucketID, ServiceID: call.serviceID,
		AuthType: resolved.authType, AuthName: resolved.authName, ContractHash: contractHash,
		CredentialSourceServiceID: source.ServiceID,
		CredentialSourceAuthType:  source.AuthType,
		CredentialSourceAuthName:  source.AuthName,
		EndUserRef:                endUserRef, TokenHash: connectHash(token), CreatedByAppID: createdByAppID,
		ReturnURL: returnURL, ResourceInputJSON: canonical, RequestedScopes: scopes, ExpiresAt: expiresAt,
	}); err != nil {
		return connectSessionStartResponse{}, err
	}
	return connectSessionStartResponse{
		AuthorizeURL: inputURL, ExpiresAt: expiresAt, ConnectSessionID: correlationID, Scopes: scopes,
		Route: "hosted_form", MissingFieldCount: missingFieldCount,
	}, nil
}

// connectInputContractHash pins the service version, selected auth flow, and
// resource-input profile across the short browser handoff. The hash contains
// no OAuth client credential and is never exposed in telemetry or URLs.
func connectInputContractHash(resolved connectRuntimeConfig) (string, error) {
	identity := connectInputContractIdentity{
		ServiceVersionID: resolved.metadata.ServiceVersionID.String(),
		AuthType:         resolved.authType, AuthName: resolved.authName,
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
	source := effectiveApplicationCredentialSource(call, resolved)
	correlationID := uuid.New()
	session := store.ConnectSession{
		ID:                        correlationID,
		BucketID:                  call.bucketID,
		ServiceID:                 call.serviceID,
		ServiceVersionID:          resolved.metadata.ServiceVersionID,
		AuthType:                  resolved.authType,
		AuthName:                  resolved.authName,
		CredentialSourceServiceID: source.ServiceID,
		CredentialSourceAuthType:  source.AuthType,
		CredentialSourceAuthName:  source.AuthName,
		RedirectURI:               resolved.credentials.RedirectURI,
		EndUserRef:                endUserRef,
		StateHash:                 connectHash(state),
		NonceHash:                 connectHash(nonce),
		EncryptedDEK:              encrypted.wrappedDEK,
		EncryptedPKCEVerifier:     encrypted.value,
		CreatedByAppID:            createdByAppID,
		ReturnURL:                 returnURL,
		ResourceInputJSON:         resourceInputJSON,
		RequestedScopes:           scopes,
		ExpiresAt:                 expiresAt,
	}
	return session, connectSessionStartResponse{AuthorizeURL: authURL, ExpiresAt: expiresAt, ConnectSessionID: correlationID, Scopes: scopes, Route: "direct"}, nil
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

// applyAppConnectScopePolicy enforces the already-loaded immutable app ceiling without another runtime query.
func applyAppConnectScopePolicy(policy, requested []string) ([]string, error) {
	// A direct CLI request or an app without a scope ceiling keeps the provider-declared scope behavior.
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
		BucketID:                  session.BucketID,
		ServiceID:                 session.ServiceID,
		ServiceVersionID:          session.ServiceVersionID,
		EndUserRef:                session.EndUserRef,
		CreatedByAppID:            session.CreatedByAppID,
		AuthType:                  resolved.authType,
		AuthName:                  resolved.authName,
		CredentialSourceServiceID: session.CredentialSourceServiceID,
		CredentialSourceAuthType:  session.CredentialSourceAuthType,
		CredentialSourceAuthName:  session.CredentialSourceAuthName,
		EncryptedDEK:              wrappedDEK,
		EncryptedAccessToken:      access,
		EncryptedRefreshToken:     refresh,
		EncryptedIDToken:          idToken,
		TokenType:                 connectauth.DefaultTokenType(token.TokenType),
		Scopes:                    scopeSet.Scopes,
		ScopeSource:               scopeSet.Source,
		Issuer:                    connectauth.ClaimString(claims, "iss"),
		Subject:                   connectauth.ClaimString(claims, "sub"),
		IdentityClaims:            connectauth.ClaimBytes(claims),
		ExpiresAt:                 connectauth.TokenExpiresAt(token.ExpiresIn),
		RefreshTokenExpiresAt:     connectauth.RefreshTokenExpiresAt(token.RefreshTokenExpiresIn),
		RefreshState:              "ok",
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
	recordConnectCallbackError(ctx, err)
	// A missing return destination keeps the failure on the Engine-owned page.
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
	// Callback query parameters use the same closed classifier as JSON errors;
	// provider or internal prose must never be transformed into a URL value.
	if errors.As(err, &httpErr) {
		return connectRuntimeErrorCode(httpErr)
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
	recordConnectCallbackError(ctx, err)
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

// recordConnectCallbackError adds only the stable callback classifier to the
// existing callback span and never records provider or token failure prose.
func recordConnectCallbackError(ctx context.Context, err error) {
	code := connectCallbackErrorCode(err)
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("error.code", code))
	span.SetStatus(codes.Error, code)
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
	code          string
	message       string
	publicMessage string
	remediation   string
}

// Error lets connect helpers preserve HTTP-safe failure messages without
// leaking lower-level token/client details to the browser.
func (e connectRuntimeHTTPError) Error() string { return e.message }

// writeConnectRuntimeError maps start-session failures into the shared
// structured control-plane envelope while keeping internal causes private.
func writeConnectRuntimeError(w http.ResponseWriter, ctx context.Context, err error, phase, commitState string) {
	var httpErr connectRuntimeHTTPError
	// Reviewed runtime errors retain safe status and message details for callers.
	if errors.As(err, &httpErr) {
		remediation := connectRuntimeErrorRemediation(httpErr.status)
		// Reviewed configuration failures can direct operators to the exact missing startup setting.
		if strings.TrimSpace(httpErr.remediation) != "" {
			remediation = httpErr.remediation
		}
		// An ambiguous session creation must be investigated before another mutation is attempted.
		if commitState == "unknown" {
			remediation = "Use the request or trace ID to inspect Engine logs before retrying."
		}
		writeWorkspaceConfigError(w, workspaceConfigHTTPError{
			status: httpErr.status, code: connectRuntimeErrorCode(httpErr), message: httpErr.message,
			category: connectRuntimeErrorCategory(httpErr.status), remediation: remediation,
			phase: phase, commitState: commitState,
		}, ctx)
		return
	}
	// Unknown causes are retained only for cancellation classification and never serialized.
	writeWorkspaceConfigError(w, workspaceConfigHTTPError{
		status: http.StatusInternalServerError, code: "connect_runtime_failed",
		message: "The Engine could not start the connection.", category: "internal", retryable: true,
		remediation: "Use the request or trace ID to inspect Engine logs before retrying.",
		phase:       phase, commitState: commitState, cause: err,
	}, ctx)
}

var connectRuntimeExactErrorCodes = map[string]string{
	"OAuth application credentials not found":            "oauth_application_credentials_not_found",
	"connect service version changed":                    "connect_service_version_changed",
	"resource input is not supported":                    "connect_resource_input_unsupported",
	"resource input is invalid":                          "connect_resource_input_invalid",
	"app scope is unavailable":                           "connect_app_scope_unavailable",
	"app scope is invalid":                               "connect_app_scope_invalid",
	"scopes must contain at least one non-empty value":   "connect_scopes_required",
	"OIDC connect scopes must include openid":            "connect_openid_scope_required",
	"state and code are required":                        "connect_callback_parameters_required",
	"connect session expired or already used":            "connect_session_unavailable",
	"token exchange failed":                              "connect_token_exchange_failed",
	"provider did not issue a required refresh token":    "connect_refresh_token_required",
	"authorization_url is required":                      "connect_authorization_url_required",
	"authorization_url must be absolute":                 "invalid_connect_authorization_url",
	"connect requires the authorizationCode OAuth2 flow": "connect_authorization_code_flow_required",
	"selected OAuth2 flow requires token_url":            "connect_token_url_required",
	"selected OAuth2 flow requires authorization_url":    "connect_authorization_url_required",
	"connect auth configuration changed":                 "connect_auth_configuration_changed",
	"connect session service version is unavailable":     "connect_service_version_unavailable",
	"failed to store auth connection":                    "connect_connection_store_failed",
}

// connectRuntimeErrorCode returns bounded stable identifiers independently of
// any provider, customer, or internal value embedded in the safe message.
func connectRuntimeErrorCode(err connectRuntimeHTTPError) string {
	// An explicit handler-edge code is authoritative for that reviewed validation path.
	if strings.TrimSpace(err.code) != "" {
		return err.code
	}
	// Exact known messages keep distinct recovery semantics without exposing them as telemetry keys.
	if code := connectRuntimeExactErrorCodes[strings.TrimSpace(err.message)]; code != "" {
		return code
	}
	return connectRuntimeVariableErrorCode(err.status, err.message)
}

// connectRuntimeVariableErrorCode classifies the few safe diagnostics that
// contain a requested scope or configured auth family without copying values.
func connectRuntimeVariableErrorCode(status int, message string) string {
	trimmed := strings.TrimSpace(message)
	// App-policy scope rejection is distinct from provider-contract scope rejection.
	if strings.Contains(trimmed, "outside the app policy") {
		return "connect_scope_outside_app_policy"
	}
	// Provider-contract scope rejection tells callers to select only declared scopes.
	if strings.Contains(trimmed, "not declared by this service") {
		return "connect_scope_not_declared"
	}
	// Auth-name mismatches require configuration repair rather than request retries.
	if strings.HasPrefix(trimmed, "service has no configured auth_name") {
		return "connect_auth_config_not_found"
	}
	// Provider callback denial remains a stable class regardless of provider prose.
	if strings.HasPrefix(trimmed, "provider returned error:") {
		return "connect_provider_denied"
	}
	return connectRuntimeStatusErrorCode(status)
}

// connectRuntimeStatusErrorCode provides a stable fallback when a future safe
// diagnostic has not yet gained a more precise reviewed code.
func connectRuntimeStatusErrorCode(status int) string {
	// HTTP families provide deterministic fallback codes without depending on mutable prose.
	switch status {
	case http.StatusBadRequest:
		return "invalid_connect_request"
	case http.StatusUnauthorized:
		return "connect_authentication_required"
	case http.StatusForbidden:
		return "connect_permission_denied"
	case http.StatusNotFound:
		return "connect_resource_not_found"
	case http.StatusConflict:
		return "connect_configuration_conflict"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "connect_dependency_unavailable"
	default:
		return "connect_runtime_failed"
	}
}

// connectRuntimeErrorCategory marks upstream failures as dependencies while
// allowing the shared writer to derive ordinary status categories.
func connectRuntimeErrorCategory(status int) string {
	// Gateway statuses represent provider or Registry dependencies, not Engine validation.
	if status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		return "dependency"
	}
	return ""
}

// connectRuntimeErrorRemediation gives each status family one safe recovery
// action without repeating request values or internal failure detail.
func connectRuntimeErrorRemediation(status int) string {
	// Recovery follows the HTTP ownership boundary: caller, authorization, configuration, dependency, or Engine.
	switch status {
	case http.StatusBadRequest:
		return "Correct the request fields or service connection policy and retry."
	case http.StatusUnauthorized:
		return "Log in or provide a valid Fused credential before retrying."
	case http.StatusForbidden:
		return "Use an authorized app and bucket binding with the required connect scope."
	case http.StatusNotFound:
		return "Store the complete OAuth/OIDC application credential pair in the selected bucket with secret set."
	case http.StatusConflict:
		return "Reapply the app or service configuration before starting a new connection."
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "Retry after the provider or Registry dependency is available."
	default:
		return "Retry and use the request or trace ID to inspect Engine logs if the problem continues."
	}
}
