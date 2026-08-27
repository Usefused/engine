package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/connectauth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

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
	bucketID         uuid.UUID
	serviceID        uuid.UUID
	authType         string
	authName         string
	authRef          string
	credentialSource connectauth.ApplicationCredentialSource
	appConnectScopes []string
}

type bucketAdminCall struct {
	bucketID uuid.UUID
}

// resolveConnectAdminCall validates the exact bucket and service identity used by connect sessions.
func resolveConnectAdminCall(w http.ResponseWriter, r *http.Request, s store.Store) (connectAdminCall, bool) {
	return resolveConnectAdminCallForPhase(w, r, s, "")
}

// resolveConnectAdminCallForPhase resolves one connect route with optional mutation certainty metadata.
func resolveConnectAdminCallForPhase(w http.ResponseWriter, r *http.Request, s store.Store, phase string) (connectAdminCall, bool) {
	bucketCall, ok := resolveBucketAdminCallForPhase(w, r, s, phase)
	// Bucket admission must succeed before the service identity can be trusted.
	if !ok {
		return connectAdminCall{}, false
	}
	serviceID, ok := parseUUIDParamForPhase(w, r, "service_id", phase)
	// A malformed service route cannot identify an exact consent target.
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
		writeConnectAdminAdmissionError(w, r.Context(), http.StatusUnauthorized, "authentication_required", "Authentication is required to manage connections.", "Log in or provide a valid Fused credential.", phase)
		return bucketAdminCall{}, false
	}
	// Workspace verification must finish before a bucket route is admitted.
	if err := verifyWorkspaceActor(r.Context(), accountID); err != nil {
		writeConnectAdminAdmissionError(w, r.Context(), http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for this connection.", "Retry and check Engine logs if the problem continues.", phase)
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
