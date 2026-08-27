package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// connectConfigUpsertPayload fields are pointers so a partial update (e.g.
// rotating just redirect_uri) can distinguish "not provided, leave
// unchanged" (nil) from "provided as blank" (empty string, a validation
// error) -- see resolveConnectConfigFields for how omitted fields are
// carried forward from the existing row instead of being required every call.
type connectConfigUpsertPayload struct {
	AuthType     *string `json:"auth_type"`
	AuthName     *string `json:"auth_name"`
	Enabled      *bool   `json:"enabled"`
	ClientID     *string `json:"client_id"`
	ClientSecret *string `json:"client_secret"`
	RedirectURI  *string `json:"redirect_uri"`
}

type connectConfigResponse struct {
	ID              uuid.UUID `json:"id"`
	BucketID        uuid.UUID `json:"bucket_id"`
	ServiceID       uuid.UUID `json:"service_id"`
	AuthType        string    `json:"auth_type"`
	AuthName        string    `json:"auth_name"`
	Enabled         bool      `json:"enabled"`
	RedirectURI     string    `json:"redirect_uri"`
	HasClientID     bool      `json:"has_client_id"`
	HasClientSecret bool      `json:"has_client_secret"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type authConnectionResponse struct {
	ID                    uuid.UUID  `json:"id"`
	BucketID              uuid.UUID  `json:"bucket_id"`
	ServiceID             uuid.UUID  `json:"service_id"`
	ServiceVersionID      *uuid.UUID `json:"service_version_id,omitempty"`
	EndUserRef            string     `json:"end_user_ref"`
	CreatedByAppID        uuid.UUID  `json:"created_by_app_id,omitempty"`
	AuthType              string     `json:"auth_type"`
	AuthName              string     `json:"auth_name"`
	TokenType             string     `json:"token_type"`
	Scopes                []string   `json:"scopes"`
	ScopeSource           string     `json:"scope_source"`
	Issuer                string     `json:"issuer,omitempty"`
	Subject               string     `json:"subject,omitempty"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt *time.Time `json:"refresh_token_expires_at,omitempty"`
	LastUsedAt            *time.Time `json:"last_used_at,omitempty"`
	LastRefreshAttemptAt  *time.Time `json:"last_refresh_attempt_at,omitempty"`
	LastRefreshedAt       *time.Time `json:"last_refreshed_at,omitempty"`
	RefreshRetryNotBefore *time.Time `json:"refresh_retry_not_before,omitempty"`
	RefreshState          string     `json:"refresh_state"`
	LastFailureCode       string     `json:"last_failure_code,omitempty"`
	LastFailureAt         *time.Time `json:"last_failure_at,omitempty"`
	LastFailureTraceID    string     `json:"last_failure_trace_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// UpsertConnectConfigHandler merges, validates, encrypts, and saves one bucket
// registration while keeping credentials out of diagnostics.
func UpsertConnectConfigHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.connect_config.upsert")
		defer span.End()

		call, ok := resolveConnectAdminMutationCall(w, r, s, "connect_config_upsert")
		if !ok {
			return
		}

		var payload connectConfigUpsertPayload
		// Invalid JSON cannot safely drive partial credential merge semantics.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_connect_config_request", "The connect configuration request body is invalid.", "Check auth_type, auth_name, enabled, client credentials, and redirect_uri.", "connect_config_upsert", "", "not_committed", "")
			return
		}

		// Read-before-write: a partial update (e.g. just rotating redirect_uri)
		// needs the existing encrypted client_id/client_secret to carry forward
		// unchanged, and there is no other way to "not touch" an encrypted
		// column -- the admin response never returns decrypted values (see
		// connectConfigResponse), so a caller cannot resend what it was never
		// given back. One read, one write below; never a loop over rows.
		existing, err := s.GetConnectConfig(ctx, call.bucketID, call.serviceID)
		// Existing encrypted fields must be available before a safe partial update.
		if err != nil {
			slog.ErrorContext(ctx, "failed to load existing connect config", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "connect_config_load_failed", "The Engine could not load the existing connect configuration.", "Retry and check Engine logs if the problem continues.", "connect_config_upsert", "", "not_committed", "")
			return
		}

		payload.normalize()
		// Validation runs against the merged shape so partial updates cannot erase required fields.
		if msg := validateConnectConfigPayload(&payload, existing); msg != "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_connect_config", msg, "Provide the required OAuth or OIDC registration fields and retry.", "connect_config_upsert", "", "not_committed", "")
			return
		}

		resolved, err := resolveConnectConfigFields(payload, existing, masterKey)
		// Decryption or merge failures remain opaque to protect stored credentials.
		if err != nil {
			slog.ErrorContext(ctx, "failed to resolve connect config fields", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "connect_config_resolution_failed", "The Engine could not resolve the existing connect configuration.", "Check Engine master-key configuration and retry.", "connect_config_upsert", "", "not_committed", "")
			return
		}
		cfg, err := encryptConnectConfig(call.bucketID, call.serviceID, resolved, masterKey)
		// Encryption must finish before persistence receives the registration.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "connect_config_encryption_failed", "The Engine could not encrypt the connect configuration.", "Check Engine master-key configuration and retry.", "connect_config_upsert", "", "not_committed", "")
			return
		}

		// Create vs. update is recorded for audit only as which action ran --
		// never the field values -- so a trail exists ("was a new OAuth app
		// registered, or an existing one rotated") without ever putting a
		// credential anywhere near telemetry.
		span.SetAttributes(connectAdminAttrs(connectConfigUpsertAction(existing), call)...)
		saved, err := s.UpsertConnectConfig(ctx, cfg)
		// An unclassified repository failure cannot prove whether the save committed.
		if err != nil {
			slog.ErrorContext(ctx, "failed to upsert connect config", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "connect_config_save_failed", "The Engine could not save the connect configuration.", "Inspect the current connect configuration before retrying, and use the request or trace ID to check Engine logs.", "connect_config_upsert", "", "unknown", "")
			return
		}
		span.SetAttributes(attribute.String("outcome", "upserted"))
		writeConnectJSON(w, http.StatusOK, projectConnectConfig(saved))
	}
}

// connectConfigUpsertAction names the OTEL action distinctly for create vs.
// update so an audit trail can tell them apart at a glance.
func connectConfigUpsertAction(existing *store.ConnectConfig) string {
	if existing == nil {
		return "connect_config.create"
	}
	return "connect_config.update"
}

// GetConnectConfigHandler is a plain read -- no state changes, so unlike
// UpsertConnectConfigHandler it carries no OTEL audit span; the CODE
// REQUIREMENT to trace user/agent-triggered execution applies to mutations,
// which this is not. It reuses the exact same safe projection
// (projectConnectConfig) the upsert response already returns, so a caller
// checking "did my last `connect set` actually take effect" sees the
// identical shape either way -- never a decrypted client_id/client_secret,
// only auth_type/enabled/redirect_uri plus has_client_id/has_client_secret.
func GetConnectConfigHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call, ok := resolveConnectAdminCall(w, r, s)
		if !ok {
			return
		}
		cfg, err := s.GetConnectConfig(r.Context(), call.bucketID, call.serviceID)
		// Store failures are distinct from an authoritative absent configuration.
		if err != nil {
			slog.ErrorContext(r.Context(), "failed to load connect config", slog.Any("error", err))
			writeControlAPIError(w, r.Context(), http.StatusInternalServerError, "connect_config_load_failed", "The Engine could not load the connect configuration.", "Retry and check Engine logs if the problem continues.")
			return
		}
		// Nil is the authoritative not-configured state used by `connect get`.
		if cfg == nil {
			writeControlAPIError(w, r.Context(), http.StatusNotFound, "connect_config_not_found", "No connect configuration exists for this bucket and service.", "Create it with `fused-cli connect set`.")
			return
		}
		writeConnectJSON(w, http.StatusOK, projectConnectConfig(cfg))
	}
}

// DeleteAuthConnectionHandler removes one connected-user grant from the exact
// bucket authorized by the control route.
func DeleteAuthConnectionHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.auth_connections.delete")
		defer span.End()

		call, ok := resolveBucketAdminMutationCall(w, r, s, "auth_connection_delete")
		if !ok {
			return
		}
		connectionID, ok := parseUUIDMutationParam(w, r, "connection_id", "auth_connection_delete")
		if !ok {
			return
		}
		span.SetAttributes(connectBucketAdminAttrs("auth_connection.delete", call, connectionID)...)
		// Known absence is safe to expose; unknown store failures remain opaque.
		if err := s.DeleteAuthConnection(ctx, call.bucketID, connectionID); err != nil {
			// A missing grant is caller-remediable and should not suggest a retryable outage.
			if errors.Is(err, store.ErrAuthConnectionNotFound) {
				writeControlAPIMutationError(w, ctx, http.StatusNotFound, "auth_connection_not_found", "The selected auth connection was not found.", "Refresh connected users before retrying.", "auth_connection_delete", "", "not_committed", "")
				return
			}
			slog.ErrorContext(ctx, "failed to delete auth connection", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "auth_connection_delete_failed", "The Engine could not delete the auth connection.", "Inspect connected users before retrying, and use the request or trace ID to check Engine logs.", "auth_connection_delete", "", "unknown", "")
			return
		}
		span.SetAttributes(attribute.String("outcome", "deleted"))
		w.WriteHeader(http.StatusNoContent)
	}
}

type connectAdminCall struct {
	bucketID  uuid.UUID
	serviceID uuid.UUID
}

type bucketAdminCall struct {
	bucketID uuid.UUID
}

// resolveConnectAdminCall validates the exact bucket and service route identity
// shared by connect configuration reads and writes.
func resolveConnectAdminCall(w http.ResponseWriter, r *http.Request, s store.Store) (connectAdminCall, bool) {
	return resolveConnectAdminCallForPhase(w, r, s, "")
}

// resolveConnectAdminMutationCall validates bucket and service route identity
// while preserving mutation outcome metadata for every admission failure.
func resolveConnectAdminMutationCall(w http.ResponseWriter, r *http.Request, s store.Store, phase string) (connectAdminCall, bool) {
	return resolveConnectAdminCallForPhase(w, r, s, phase)
}

// resolveConnectAdminCallForPhase owns the shared route-resolution algorithm;
// a non-empty phase adds mutation certainty without duplicating admission.
func resolveConnectAdminCallForPhase(w http.ResponseWriter, r *http.Request, s store.Store, phase string) (connectAdminCall, bool) {
	bucketCall, ok := resolveBucketAdminCallForPhase(w, r, s, phase)
	// Bucket admission must succeed before the service identity can be trusted.
	if !ok {
		return connectAdminCall{}, false
	}
	serviceID, ok := parseUUIDParamForPhase(w, r, "service_id", phase)
	// A malformed service route cannot identify an exact connect configuration.
	if !ok {
		return connectAdminCall{}, false
	}
	return connectAdminCall{
		bucketID:  bucketCall.bucketID,
		serviceID: serviceID,
	}, true
}

// resolveBucketAdminCall authenticates the actor and verifies the exact route
// bucket before any bucket-scoped connect operation proceeds.
func resolveBucketAdminCall(w http.ResponseWriter, r *http.Request, s store.Store) (bucketAdminCall, bool) {
	return resolveBucketAdminCallForPhase(w, r, s, "")
}

// resolveBucketAdminMutationCall authenticates and validates a bucket-scoped
// mutation before storage writes, reporting every rejection as not committed.
func resolveBucketAdminMutationCall(w http.ResponseWriter, r *http.Request, s store.Store, phase string) (bucketAdminCall, bool) {
	return resolveBucketAdminCallForPhase(w, r, s, phase)
}

// resolveBucketAdminCallForPhase centralizes authentication and exact bucket
// admission for both reads and mutations without adding repository queries.
func resolveBucketAdminCallForPhase(w http.ResponseWriter, r *http.Request, s store.Store, phase string) (bucketAdminCall, bool) {
	accountID, err := controlActorAccount(r.Context())
	// Missing actor identity rejects bucket administration before repository access.
	if err != nil {
		writeConnectAdminAdmissionError(w, r.Context(), http.StatusUnauthorized, "authentication_required", "Authentication is required to manage connect configuration.", "Log in or provide a valid Fused credential.", phase)
		return bucketAdminCall{}, false
	}
	// Workspace verification must finish before a bucket route is admitted.
	if err := verifyWorkspaceActor(r.Context(), accountID); err != nil {
		writeConnectAdminAdmissionError(w, r.Context(), http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for connect configuration.", "Retry and check Engine logs if the problem continues.", phase)
		return bucketAdminCall{}, false
	}
	bucketID, ok := parseUUIDParamForPhase(w, r, "bucket_id", phase)
	// An invalid route identity cannot reach bucket storage.
	if !ok {
		return bucketAdminCall{}, false
	}
	// Exact bucket lookup proves workspace membership before route admission.
	if _, err := s.GetBucket(r.Context(), bucketID); err != nil {
		writeBucketLookupErrorForPhase(r.Context(), w, err, phase)
		return bucketAdminCall{}, false
	}
	return bucketAdminCall{bucketID: bucketID}, true
}

// resolvedConnectConfigFields holds the fully-resolved plaintext values --
// merged from whatever the caller provided plus whatever survives from the
// existing row -- right before re-encryption. Keeping this as its own type
// (rather than reusing connectConfigUpsertPayload, whose fields are pointers
// for partial-update purposes) means encryptConnectConfig never has to
// re-derive "was this provided" logic; that question is answered exactly
// once, in resolveConnectConfigFields.
type resolvedConnectConfigFields struct {
	AuthType     string
	AuthName     string
	Enabled      bool
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func encryptConnectConfig(bucketID, serviceID uuid.UUID, resolved resolvedConnectConfigFields, masterKey []byte) (store.ConnectConfig, error) {
	// A fresh DEK is generated on every save, including partial updates that
	// only touch redirect_uri -- simpler than trying to reuse the prior DEK,
	// and re-encrypting an unchanged plaintext under a new DEK is exactly as
	// safe as leaving it alone.
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	encryptedClientID, err := store.EncryptWithDEK(dek, resolved.ClientID)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	encryptedClientSecret, err := store.EncryptWithDEK(dek, resolved.ClientSecret)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	return store.ConnectConfig{
		BucketID:              bucketID,
		ServiceID:             serviceID,
		AuthType:              resolved.AuthType,
		AuthName:              resolved.AuthName,
		Enabled:               resolved.Enabled,
		EncryptedDEK:          wrappedDEK,
		EncryptedClientID:     encryptedClientID,
		EncryptedClientSecret: encryptedClientSecret,
		RedirectURI:           resolved.RedirectURI,
	}, nil
}

// resolveConnectConfigFields merges whatever the caller explicitly sent over
// whatever the existing row (if any) already had, so a caller can rotate one
// field without needing to know or resend the others. existing is nil on
// first-time creation; validateConnectConfigPayload has already guaranteed
// every field is present in that case, so there is nothing to merge.
func resolveConnectConfigFields(payload connectConfigUpsertPayload, existing *store.ConnectConfig, masterKey []byte) (resolvedConnectConfigFields, error) {
	resolved, err := existingConnectConfigFields(existing, masterKey)
	if err != nil {
		return resolvedConnectConfigFields{}, err
	}
	if payload.AuthType != nil {
		resolved.AuthType = *payload.AuthType
	}
	if payload.AuthName != nil {
		resolved.AuthName = *payload.AuthName
	}
	if payload.Enabled != nil {
		resolved.Enabled = *payload.Enabled
	} else if existing == nil {
		resolved.Enabled = true
	}
	if payload.ClientID != nil {
		resolved.ClientID = *payload.ClientID
	}
	if payload.ClientSecret != nil {
		resolved.ClientSecret = *payload.ClientSecret
	}
	if payload.RedirectURI != nil {
		resolved.RedirectURI = *payload.RedirectURI
	}
	return resolved, nil
}

// existingConnectConfigFields decrypts a prior row's client_id/client_secret
// so an update that omits them can carry the values forward unchanged. The
// admin API never returns decrypted values to a caller (see
// connectConfigResponse's HasClientID/HasClientSecret flags), so this is the
// only place that can recover them. Returns the zero value, not an error,
// when there is nothing to carry forward.
func existingConnectConfigFields(existing *store.ConnectConfig, masterKey []byte) (resolvedConnectConfigFields, error) {
	if existing == nil {
		return resolvedConnectConfigFields{}, nil
	}
	dek, err := store.UnwrapDEK(masterKey, existing.EncryptedDEK)
	if err != nil {
		return resolvedConnectConfigFields{}, fmt.Errorf("unwrap existing connect config dek: %w", err)
	}
	clientID, err := store.DecryptWithDEK(dek, existing.EncryptedClientID)
	if err != nil {
		return resolvedConnectConfigFields{}, fmt.Errorf("decrypt existing client_id: %w", err)
	}
	clientSecret, err := store.DecryptWithDEK(dek, existing.EncryptedClientSecret)
	if err != nil {
		return resolvedConnectConfigFields{}, fmt.Errorf("decrypt existing client_secret: %w", err)
	}
	return resolvedConnectConfigFields{
		AuthType:     existing.AuthType,
		AuthName:     existing.AuthName,
		Enabled:      existing.Enabled,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURI:  existing.RedirectURI,
	}, nil
}

// normalize trims and canonicalizes only the fields the caller actually
// provided. A nil field must stay nil through this step -- that is what lets
// validateConnectConfigPayload tell "omitted, leave unchanged" apart from an
// explicit empty string, which is a validation error either way.
func (p *connectConfigUpsertPayload) normalize() {
	if p.AuthType != nil {
		canonical := canonicalWorkspaceStaticAuthType(strings.TrimSpace(*p.AuthType))
		p.AuthType = &canonical
	}
	p.AuthName = trimPtr(p.AuthName)
	p.ClientID = trimPtr(p.ClientID)
	p.ClientSecret = trimPtr(p.ClientSecret)
	p.RedirectURI = trimPtr(p.RedirectURI)
}

// trimPtr trims a provided value without collapsing "not provided" (nil)
// into "provided as blank" -- the two must stay distinguishable through
// normalization for partial-update validation to work.
func trimPtr(value *string) *string {
	if value == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*value)
	return &trimmed
}

// validateConnectConfigPayload enforces shape rules that differ by whether a
// config already exists: creating one needs every field (there is nothing to
// fall back to), while updating one only needs whichever fields the caller
// actually sent -- GetConnectConfig's read plus resolveConnectConfigFields'
// merge supplies the rest.
func validateConnectConfigPayload(payload *connectConfigUpsertPayload, existing *store.ConnectConfig) string {
	if existing == nil {
		return validateConnectConfigCreate(payload)
	}
	return validateConnectConfigUpdate(payload)
}

func validateConnectConfigCreate(payload *connectConfigUpsertPayload) string {
	checks := []string{
		validateConnectAuthType(payload.AuthType),
		requiredConnectValue(payload.AuthName, "auth_name"),
		requiredConnectValue(payload.ClientID, "client_id"),
		requiredConnectValue(payload.ClientSecret, "client_secret"),
		validateConnectRedirect(payload.RedirectURI),
	}
	for _, message := range checks {
		if message != "" {
			return message
		}
	}
	return ""
}

// validateConnectConfigUpdate requires at least one field so a no-op PUT
// cannot silently "succeed" and confuse a caller checking whether their
// change took effect; every other rule only applies to a field that was
// actually provided; the rest are left to resolveConnectConfigFields to
// carry forward from the existing row.
func validateConnectConfigUpdate(payload *connectConfigUpsertPayload) string {
	if connectUpdateEmpty(payload) {
		return "update requires at least one of auth_type, auth_name, client_id, client_secret, redirect_uri, or enabled"
	}
	checks := []string{
		validateOptionalConnectAuthType(payload.AuthType),
		validateOptionalConnectValue(payload.AuthName, "auth_name"),
		validateOptionalConnectValue(payload.ClientID, "client_id"),
		validateOptionalConnectValue(payload.ClientSecret, "client_secret"),
		validateOptionalConnectRedirect(payload.RedirectURI),
	}
	for _, message := range checks {
		if message != "" {
			return message
		}
	}
	return ""
}

func connectUpdateEmpty(payload *connectConfigUpsertPayload) bool {
	return payload.AuthType == nil && payload.AuthName == nil && payload.ClientID == nil && payload.ClientSecret == nil && payload.RedirectURI == nil && payload.Enabled == nil
}

func validateConnectAuthType(value *string) string {
	if value == nil || !isSupportedConnectAuthType(*value) {
		return "unsupported auth_type"
	}
	return ""
}

func validateOptionalConnectAuthType(value *string) string {
	if value == nil {
		return ""
	}
	return validateConnectAuthType(value)
}

func requiredConnectValue(value *string, name string) string {
	if value == nil || *value == "" {
		return name + " is required"
	}
	return ""
}

func validateOptionalConnectValue(value *string, name string) string {
	if value != nil && *value == "" {
		return name + " cannot be blanked out -- omit it to leave unchanged"
	}
	return ""
}

func validateConnectRedirect(value *string) string {
	if value == nil || !isHTTPRedirectURI(*value) {
		return "redirect_uri must be an absolute http or https URL"
	}
	return ""
}

func validateOptionalConnectRedirect(value *string) string {
	if value == nil {
		return ""
	}
	return validateConnectRedirect(value)
}

// isSupportedConnectAuthType keeps connect admin scoped to flows the connection
// table can satisfy; static credentials are managed through bucket secrets.
func isSupportedConnectAuthType(authType string) bool {
	switch canonicalWorkspaceStaticAuthType(authType) {
	case "oauth", "oidc":
		return true
	default:
		return false
	}
}

// isHTTPRedirectURI rejects relative or non-browser redirect targets because
// provider callbacks must land on an explicit Engine/app URL.
func isHTTPRedirectURI(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.Host != "" && (parsed.Scheme == "http" || parsed.Scheme == "https")
}

func projectConnectConfig(cfg *store.ConnectConfig) connectConfigResponse {
	return connectConfigResponse{
		ID:              cfg.ID,
		BucketID:        cfg.BucketID,
		ServiceID:       cfg.ServiceID,
		AuthType:        cfg.AuthType,
		AuthName:        cfg.AuthName,
		Enabled:         cfg.Enabled,
		RedirectURI:     cfg.RedirectURI,
		HasClientID:     cfg.EncryptedClientID != "",
		HasClientSecret: cfg.EncryptedClientSecret != "",
		CreatedAt:       cfg.CreatedAt,
		UpdatedAt:       cfg.UpdatedAt,
	}
}

// projectAuthConnection exposes version-pinned refresh lifecycle metadata while
// deliberately omitting encrypted tokens and private lease ownership fields.
func projectAuthConnection(conn store.AuthConnection) authConnectionResponse {
	return authConnectionResponse{
		ID:                    conn.ID,
		BucketID:              conn.BucketID,
		ServiceID:             conn.ServiceID,
		ServiceVersionID:      optionalConnectionUUID(conn.ServiceVersionID),
		EndUserRef:            conn.EndUserRef,
		CreatedByAppID:        conn.CreatedByAppID,
		AuthType:              conn.AuthType,
		AuthName:              conn.AuthName,
		TokenType:             conn.TokenType,
		Scopes:                conn.Scopes,
		ScopeSource:           conn.ScopeSource,
		Issuer:                conn.Issuer,
		Subject:               conn.Subject,
		ExpiresAt:             conn.ExpiresAt,
		RefreshTokenExpiresAt: conn.RefreshTokenExpiresAt,
		LastUsedAt:            conn.LastUsedAt,
		LastRefreshAttemptAt:  conn.LastRefreshAttemptAt,
		LastRefreshedAt:       conn.LastRefreshedAt,
		RefreshRetryNotBefore: conn.RefreshRetryNotBefore,
		RefreshState:          conn.RefreshState,
		LastFailureCode:       conn.LastFailureCode,
		LastFailureAt:         conn.LastFailureAt,
		LastFailureTraceID:    conn.LastFailureTraceID,
		CreatedAt:             conn.CreatedAt,
		UpdatedAt:             conn.UpdatedAt,
	}
}

// optionalConnectionUUID keeps ambiguous legacy service-version identity null
// instead of projecting the all-zero UUID as if it were a real contract pin.
func optionalConnectionUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// parseUUIDParam validates an exact route UUID and emits a stable field-specific
// diagnostic without reflecting the supplied value.
func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	return parseUUIDParamForPhase(w, r, name, "")
}

// parseUUIDMutationParam validates an exact route UUID and records malformed
// mutation identity as an authoritative pre-write rejection.
func parseUUIDMutationParam(w http.ResponseWriter, r *http.Request, name, phase string) (uuid.UUID, bool) {
	return parseUUIDParamForPhase(w, r, name, phase)
}

// parseUUIDParamForPhase validates route identity once and selects read or
// authoritative pre-write error metadata from the supplied phase.
func parseUUIDParamForPhase(w http.ResponseWriter, r *http.Request, name, phase string) (uuid.UUID, bool) {
	value := chi.URLParam(r, name)
	id, err := uuid.Parse(value)
	// Malformed route values never reach bucket or connection storage.
	if err != nil {
		writeConnectAdminAdmissionError(w, r.Context(), http.StatusBadRequest, "invalid_"+name, "The "+name+" route value is not a valid UUID.", "Use an ID returned by the corresponding Fused list command.", phase)
		return uuid.Nil, false
	}
	return id, true
}

// writeBucketLookupError distinguishes authoritative absence from a retryable
// store failure without exposing database details.
func writeBucketLookupError(ctx context.Context, w http.ResponseWriter, err error) {
	writeBucketLookupErrorForPhase(ctx, w, err, "")
}

// writeBucketMutationLookupError distinguishes authoritative bucket absence
// from lookup failure while proving both occurred before the requested write.
func writeBucketMutationLookupError(ctx context.Context, w http.ResponseWriter, err error, phase string) {
	writeBucketLookupErrorForPhase(ctx, w, err, phase)
}

// writeBucketLookupErrorForPhase maps bucket lookup results once while a
// non-empty phase preserves authoritative pre-write mutation metadata.
func writeBucketLookupErrorForPhase(ctx context.Context, w http.ResponseWriter, err error, phase string) {
	// A missing bucket is caller-remediable and, for mutations, proves pre-write rejection.
	if errors.Is(err, store.ErrBucketNotFound) {
		writeConnectAdminAdmissionError(w, ctx, http.StatusNotFound, "bucket_not_found", "The selected bucket was not found.", "Choose a bucket from `fused-cli bucket list`.", phase)
		return
	}
	writeConnectAdminAdmissionError(w, ctx, http.StatusInternalServerError, "bucket_lookup_failed", "The Engine could not resolve the selected bucket.", "Retry and check Engine logs if the problem continues.", phase)
}

// writeConnectAdminAdmissionError preserves the ordinary read envelope when
// no phase exists and adds not-committed metadata only for mutation admission.
func writeConnectAdminAdmissionError(w http.ResponseWriter, ctx context.Context, status int, code, message, remediation, phase string) {
	// An empty phase identifies a read path and must not claim mutation certainty.
	if phase == "" {
		writeControlAPIError(w, ctx, status, code, message, remediation)
		return
	}
	writeControlAPIMutationError(w, ctx, status, code, message, remediation, phase, "", "not_committed", "")
}

// verifyBucketInWorkspace confirms bucketID actually exists before a handler
// writes/deletes bucket-scoped rows, so a bogus id fails with a clean 404 (via
// writeBucketLookupError) instead of a raw FK-violation 500. Usable regardless
// of whether the caller read bucketID from a URL path param, JSON body, or
// query string.
func verifyBucketInWorkspace(ctx context.Context, s store.Store, bucketID uuid.UUID) error {
	_, err := s.GetBucket(ctx, bucketID)
	return err
}

func writeConnectJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func connectAdminAttrs(action string, call connectAdminCall) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("user_action", action),
		attribute.String("bucket_id", call.bucketID.String()),
		attribute.String("service_id", call.serviceID.String()),
	}
}

func connectBucketAdminAttrs(action string, call bucketAdminCall, connectionID uuid.UUID) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("user_action", action),
		attribute.String("bucket_id", call.bucketID.String()),
		attribute.String("connection_id", connectionID.String()),
	}
}
