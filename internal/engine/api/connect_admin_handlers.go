package api

import (
	"context"
	"encoding/json"
	"errors"
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

type connectConfigUpsertPayload struct {
	AuthType     string `json:"auth_type"`
	Enabled      *bool  `json:"enabled"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RedirectURI  string `json:"redirect_uri"`
}

type connectConfigResponse struct {
	ID              uuid.UUID `json:"id"`
	BucketID        uuid.UUID `json:"bucket_id"`
	ServiceID       uuid.UUID `json:"service_id"`
	AuthType        string    `json:"auth_type"`
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
	EndUserRef            string     `json:"end_user_ref"`
	CreatedByArtifactID   uuid.UUID  `json:"created_by_artifact_id,omitempty"`
	AuthType              string     `json:"auth_type"`
	TokenType             string     `json:"token_type"`
	Scopes                []string   `json:"scopes"`
	ScopeSource           string     `json:"scope_source"`
	Issuer                string     `json:"issuer,omitempty"`
	Subject               string     `json:"subject,omitempty"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	RefreshTokenExpiresAt *time.Time `json:"refresh_token_expires_at,omitempty"`
	LastUsedAt            *time.Time `json:"last_used_at,omitempty"`
	RefreshState          string     `json:"refresh_state"`
	LastFailureCode       string     `json:"last_failure_code,omitempty"`
	LastFailureAt         *time.Time `json:"last_failure_at,omitempty"`
	LastFailureTraceID    string     `json:"last_failure_trace_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

func UpsertConnectConfigHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.connect_config.upsert")
		defer span.End()

		call, ok := resolveConnectAdminCall(w, r, s)
		if !ok {
			return
		}

		var payload connectConfigUpsertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}
		if msg := validateConnectConfigPayload(&payload); msg != "" {
			http.Error(w, msg, http.StatusBadRequest)
			return
		}
		cfg, err := encryptConnectConfig(call.bucketID, call.serviceID, payload, masterKey)
		if err != nil {
			http.Error(w, "failed to encrypt connect config", http.StatusInternalServerError)
			return
		}

		span.SetAttributes(connectAdminAttrs("connect_config.upsert", call)...)
		saved, err := s.UpsertConnectConfig(ctx, cfg)
		if err != nil {
			slog.ErrorContext(ctx, "failed to upsert connect config", slog.Any("error", err))
			http.Error(w, "failed to save connect config", http.StatusInternalServerError)
			return
		}
		writeConnectJSON(w, http.StatusOK, projectConnectConfig(saved))
	}
}

func DeleteAuthConnectionHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.auth_connections.delete")
		defer span.End()

		call, ok := resolveBucketAdminCall(w, r, s)
		if !ok {
			return
		}
		connectionID, ok := parseUUIDParam(w, r, "connection_id")
		if !ok {
			return
		}
		span.SetAttributes(connectBucketAdminAttrs("auth_connection.delete", call, connectionID)...)
		if err := s.DeleteAuthConnection(ctx, call.bucketID, connectionID); err != nil {
			if errors.Is(err, store.ErrAuthConnectionNotFound) {
				http.Error(w, "auth connection not found", http.StatusNotFound)
				return
			}
			slog.ErrorContext(ctx, "failed to delete auth connection", slog.Any("error", err))
			http.Error(w, "failed to delete auth connection", http.StatusInternalServerError)
			return
		}
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

func resolveConnectAdminCall(w http.ResponseWriter, r *http.Request, s store.Store) (connectAdminCall, bool) {
	bucketCall, ok := resolveBucketAdminCall(w, r, s)
	if !ok {
		return connectAdminCall{}, false
	}
	serviceID, ok := parseUUIDParam(w, r, "service_id")
	if !ok {
		return connectAdminCall{}, false
	}
	return connectAdminCall{
		bucketID:  bucketCall.bucketID,
		serviceID: serviceID,
	}, true
}

func resolveBucketAdminCall(w http.ResponseWriter, r *http.Request, s store.Store) (bucketAdminCall, bool) {
	accountID, err := validateAPIKey(r.Context(), s, r.Header.Get("X-API-Key"))
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return bucketAdminCall{}, false
	}
	if err := s.VerifyWorkspaceOwner(r.Context(), accountID); err != nil {
		http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
		return bucketAdminCall{}, false
	}
	bucketID, ok := parseUUIDParam(w, r, "bucket_id")
	if !ok {
		return bucketAdminCall{}, false
	}
	if _, err := s.GetBucket(r.Context(), bucketID); err != nil {
		writeBucketLookupError(w, err)
		return bucketAdminCall{}, false
	}
	return bucketAdminCall{bucketID: bucketID}, true
}

func encryptConnectConfig(bucketID, serviceID uuid.UUID, payload connectConfigUpsertPayload, masterKey []byte) (store.ConnectConfig, error) {
	wrappedDEK, dek, err := store.WrapDEK(masterKey)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	encryptedClientID, err := store.EncryptWithDEK(dek, payload.ClientID)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	encryptedClientSecret, err := store.EncryptWithDEK(dek, payload.ClientSecret)
	if err != nil {
		return store.ConnectConfig{}, err
	}
	return store.ConnectConfig{
		BucketID:              bucketID,
		ServiceID:             serviceID,
		AuthType:              payload.AuthType,
		Enabled:               connectConfigEnabled(payload),
		EncryptedDEK:          wrappedDEK,
		EncryptedClientID:     encryptedClientID,
		EncryptedClientSecret: encryptedClientSecret,
		RedirectURI:           payload.RedirectURI,
	}, nil
}

// validateConnectConfigPayload normalizes the public admin vocabulary before
// validation so persisted connect config never stores imported OpenAPI names.
func validateConnectConfigPayload(payload *connectConfigUpsertPayload) string {
	payload.AuthType = canonicalWorkspaceStaticAuthType(payload.AuthType)
	payload.ClientID = strings.TrimSpace(payload.ClientID)
	payload.ClientSecret = strings.TrimSpace(payload.ClientSecret)
	payload.RedirectURI = strings.TrimSpace(payload.RedirectURI)

	if !isSupportedConnectAuthType(payload.AuthType) {
		return "unsupported auth_type"
	}
	if payload.ClientID == "" {
		return "client_id is required"
	}
	if payload.ClientSecret == "" {
		return "client_secret is required"
	}
	if !isHTTPRedirectURI(payload.RedirectURI) {
		return "redirect_uri must be an absolute http or https URL"
	}
	return ""
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
		Enabled:         cfg.Enabled,
		RedirectURI:     cfg.RedirectURI,
		HasClientID:     cfg.EncryptedClientID != "",
		HasClientSecret: cfg.EncryptedClientSecret != "",
		CreatedAt:       cfg.CreatedAt,
		UpdatedAt:       cfg.UpdatedAt,
	}
}

func projectAuthConnection(conn store.AuthConnection) authConnectionResponse {
	return authConnectionResponse{
		ID:                    conn.ID,
		BucketID:              conn.BucketID,
		ServiceID:             conn.ServiceID,
		EndUserRef:            conn.EndUserRef,
		CreatedByArtifactID:   conn.CreatedByArtifactID,
		AuthType:              conn.AuthType,
		TokenType:             conn.TokenType,
		Scopes:                conn.Scopes,
		ScopeSource:           conn.ScopeSource,
		Issuer:                conn.Issuer,
		Subject:               conn.Subject,
		ExpiresAt:             conn.ExpiresAt,
		RefreshTokenExpiresAt: conn.RefreshTokenExpiresAt,
		LastUsedAt:            conn.LastUsedAt,
		RefreshState:          conn.RefreshState,
		LastFailureCode:       conn.LastFailureCode,
		LastFailureAt:         conn.LastFailureAt,
		LastFailureTraceID:    conn.LastFailureTraceID,
		CreatedAt:             conn.CreatedAt,
		UpdatedAt:             conn.UpdatedAt,
	}
}

func parseUUIDParam(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	value := chi.URLParam(r, name)
	id, err := uuid.Parse(value)
	if err != nil {
		http.Error(w, "invalid "+name, http.StatusBadRequest)
		return uuid.Nil, false
	}
	return id, true
}

func writeBucketLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrBucketNotFound) {
		http.Error(w, "bucket not found", http.StatusNotFound)
		return
	}
	http.Error(w, "failed to resolve bucket", http.StatusInternalServerError)
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

func connectConfigEnabled(payload connectConfigUpsertPayload) bool {
	if payload.Enabled == nil {
		return true
	}
	return *payload.Enabled
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
