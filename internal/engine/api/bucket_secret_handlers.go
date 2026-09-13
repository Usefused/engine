package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/secretref"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// BucketSecretUpsertPayload carries only user-chosen generic secret metadata;
// bucket identity remains authoritative in the request path.
type BucketSecretUpsertPayload struct {
	KeyName   string     `json:"key_name"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// UpsertBucketSecretHandler encrypts one service-independent secret in the
// exact bucket selected by the caller.
func UpsertBucketSecretHandler(s store.Store, masterKey []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.bucket_secrets.upsert")
		defer span.End()

		status, message, ok := secretAdminWorkspace(ctx, s, r)
		// Generic secrets use the same authenticated workspace boundary as provider credentials.
		if !ok {
			writeControlAPIMutationError(w, ctx, status, secretAdminErrorCode(status), message, secretAdminRemediation(status), "bucket_secret_upsert", "", "not_committed", "")
			return
		}

		bucketIDValue := strings.TrimSpace(chi.URLParam(r, "id"))
		// The route must always carry the bucket so generic writes never fall back to a default implicitly.
		if bucketIDValue == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "bucket_id_required", "The bucket ID is required in the request path.", "Provide a bucket ID from `fused-cli bucket list`.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		bucketID, err := uuid.Parse(bucketIDValue)
		// Malformed bucket identities fail before body decoding, encryption, or storage.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_id", "The bucket ID is not a valid UUID.", "Use a bucket ID from `fused-cli bucket list`.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		// Exact lookup prevents a secret write from targeting an absent bucket.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "bucket_secret_upsert")
			return
		}

		payload, err := decodeBucketSecretUpsertPayload(r)
		// Strict decoding catches misspelled fields without ever reflecting the supplied value.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_secret_request", "The bucket secret request body is invalid.", "Provide key_name, value, and an optional future expires_at.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		storageKey, err := bucketSecretStorageKey(payload.KeyName)
		// Reference-safe names ensure the stored row can be consumed by SDK, MCP, and webhook bindings.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_secret_name", err.Error(), "Use a non-empty single reference segment without whitespace, periods, braces, or dollar signs.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		// Empty secret material is indistinguishable from a missing configuration at runtime.
		if payload.Value == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "empty_bucket_secret_value", "The bucket secret value cannot be empty.", "Provide the secret through the CLI's masked prompt or --value-stdin.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		// Already-expired material can never satisfy a runtime credential lookup.
		if payload.ExpiresAt != nil && !payload.ExpiresAt.After(time.Now()) {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_secret_expiry", "expires_at must be in the future.", "Choose a future RFC3339 expiry or omit expires_at.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}

		secret, err := encryptedWorkspaceSecret(bucketID, uuid.Nil, storageKey, "bucket_secret", payload.Value, masterKey)
		// Encryption must succeed before any generic secret metadata is persisted.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_secret_encryption_failed", "The Engine could not encrypt the bucket secret.", "Check Engine master-key configuration and retry.", "bucket_secret_upsert", "", "not_committed", "")
			return
		}
		secret.ExpiresAt = payload.ExpiresAt
		// Unclassified store failures retain an unknown commit outcome to prevent unsafe blind retries.
		if err := s.UpsertSecret(ctx, secret); err != nil {
			slog.ErrorContext(ctx, "failed to upsert bucket secret", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_secret_save_failed", "The Engine could not save the bucket secret.", "Inspect current bucket secret metadata before retrying, and use the request or trace ID to check Engine logs.", "bucket_secret_upsert", "", "unknown", "")
			return
		}

		span.SetAttributes(attribute.String("bucket_id", bucketID.String()), attribute.String("outcome", "upserted"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// decodeBucketSecretUpsertPayload accepts exactly one JSON object and rejects
// unknown fields so automation failures remain deterministic.
func decodeBucketSecretUpsertPayload(r *http.Request) (BucketSecretUpsertPayload, error) {
	var payload BucketSecretUpsertPayload
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	// The first decode must produce the complete supported request shape.
	if err := decoder.Decode(&payload); err != nil {
		return BucketSecretUpsertPayload{}, err
	}
	// A second JSON value is ambiguous and must not be silently ignored.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return BucketSecretUpsertPayload{}, errors.New("request body must contain exactly one JSON object")
	}
	return payload, nil
}

// bucketSecretStorageKey validates a name against the whole-value bucket
// reference grammar and returns its collision-safe storage namespace.
func bucketSecretStorageKey(value string) (string, error) {
	name := strings.TrimSpace(value)
	// Trimming would create a different reference than the caller supplied, so reject instead of normalizing.
	if name == "" || name != value {
		return "", errors.New("bucket secret name must be non-empty and cannot start or end with whitespace")
	}
	for _, char := range name {
		// These characters either split or escape ${bucket.<name>.secret.<key>} references.
		if unicode.IsSpace(char) || strings.ContainsRune(".${}", char) {
			return "", errors.New("bucket secret name must be one reference-safe segment")
		}
	}
	return secretref.KeyPrefix + name, nil
}
