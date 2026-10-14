package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

// secretExportTarget checks exact bucket authority before any secret lookup, including direct handler callers.
func secretExportTarget(w http.ResponseWriter, r *http.Request, permission accesscontrol.Permission) (uuid.UUID, uuid.UUID, bool) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	// Execution tokens have no control actor and cannot retrieve provider credentials.
	if !ok || actor.WorkspaceID == uuid.Nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return uuid.Nil, uuid.Nil, false
	}
	bucketID, bucketErr := uuid.Parse(chi.URLParam(r, "bucket_id"))
	secretID, secretErr := uuid.Parse(chi.URLParam(r, "secret_id"))
	// Both IDs are required; retrieval never guesses a default bucket or secret.
	if bucketErr != nil || secretErr != nil {
		http.Error(w, "invalid secret target", http.StatusBadRequest)
		return uuid.Nil, uuid.Nil, false
	}
	requirement := accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}}
	// Management and bucket use do not imply the reveal permission.
	if err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(r.Context(), actor, requirement); err != nil {
		http.Error(w, "permission denied", http.StatusForbidden)
		return uuid.Nil, uuid.Nil, false
	}
	return bucketID, secretID, true
}

// RevealBucketSecretHandler returns plaintext only on the explicitly authorized, durably audited POST route.
func RevealBucketSecretHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	// The route's control middleware buffers this response until its final audit record is durable.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.credentials.reveal")
		defer span.End()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		bucketID, secretID, ok := secretExportTarget(w, r, accesscontrol.PermissionCredentialsReveal)
		// Denials stop before encrypted material is loaded.
		if !ok {
			return
		}
		secret, err := s.GetExportableSecret(ctx, bucketID, secretID)
		// Storage failures must never fall back to a cached runtime credential.
		if err != nil {
			http.Error(w, "secret retrieval unavailable", http.StatusServiceUnavailable)
			return
		}
		// Private, unsupported, missing, and wrong-bucket rows share one response.
		if secret == nil {
			http.Error(w, "exportable secret not found", http.StatusNotFound)
			return
		}
		value, err := decryptExportableSecret(masterKey, secret)
		// Encryption errors are bounded so stored material cannot reach logs or errors.
		if err != nil {
			http.Error(w, "secret retrieval unavailable", http.StatusServiceUnavailable)
			return
		}
		writeConnectJSON(w, http.StatusOK, map[string]string{"value": value})
	}
}

// decryptExportableSecret keeps the unwrapped data key transient and avoids including material in errors.
func decryptExportableSecret(masterKey []byte, secret *store.WorkspaceSecret) (string, error) {
	dek, err := store.UnwrapDEK(masterKey, secret.EncryptedDEK)
	// Corrupt wrapped keys cannot be passed into payload decryption.
	if err != nil {
		return "", err
	}
	defer clear(dek)
	return store.DecryptWithDEK(dek, secret.EncryptedValue)
}

// SetBucketSecretExportableHandler lets credential managers change opt-in policy without needing reveal authority.
func SetBucketSecretExportableHandler(s store.Store) http.HandlerFunc {
	// Boolean-only policy changes cannot expose or replace the secret value.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.credentials.exportable")
		defer span.End()
		w.Header().Set("Cache-Control", "no-store")
		bucketID, secretID, ok := secretExportTarget(w, r, accesscontrol.PermissionCredentialsManage)
		// Unauthorized callers cannot probe secret existence or mutate policy.
		if !ok {
			return
		}
		exportable, err := decodeSecretExportPolicy(r)
		// Omitted, null, or misspelled flags must not silently revoke or grant retrieval.
		if err != nil {
			http.Error(w, "provide an exportable boolean", http.StatusBadRequest)
			return
		}
		changed, err := s.SetSecretExportable(ctx, bucketID, secretID, exportable)
		// Ambiguous persistence failures are never reported as successful policy updates.
		if err != nil {
			http.Error(w, "secret policy update failed", http.StatusInternalServerError)
			return
		}
		// Unsupported credential families cannot be opted in through this endpoint.
		if !changed {
			http.Error(w, "eligible secret not found", http.StatusNotFound)
			return
		}
		accesscontrol.MarkSecretExportPolicy(ctx, exportable)
		w.WriteHeader(http.StatusNoContent)
	}
}

// decodeSecretExportPolicy accepts one explicit boolean without accepting secret material or unknown fields.
func decodeSecretExportPolicy(r *http.Request) (bool, error) {
	var payload struct {
		Exportable *bool `json:"exportable"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	// Reject the whole oversized request rather than accepting a valid prefix.
	if err != nil || len(body) > 1024 {
		return false, errors.New("invalid export policy")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	// Malformed bodies never become implicit policy changes.
	if err := decoder.Decode(&payload); err != nil {
		return false, err
	}
	// A second value or absent flag is ambiguous and must fail closed.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || payload.Exportable == nil {
		return false, errors.New("invalid export policy")
	}
	return *payload.Exportable, nil
}
