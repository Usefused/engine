package api

import (
	"encoding/json"
	"net/http"

	"log/slog"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type UpsertBucketValuePayload struct {
	BucketID  uuid.UUID `json:"bucket_id"`
	ServiceID uuid.UUID `json:"service_id"`
	KeyName   string    `json:"key_name"`
	Location  string    `json:"location"`
	Value     string    `json:"value"`
}

// UpsertBucketValueHandler validates and stores one non-secret routing value
// while returning stable structured diagnostics for every rejected request.
func UpsertBucketValueHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.bucketvalues.upsert")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		// Bucket values require an authenticated control-plane actor.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to update a bucket value.", "Log in or provide a valid Fused credential.", "bucket_value_upsert", "", "not_committed", "")
			return
		}

		// Workspace verification prevents writes through stale or foreign identities.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for this bucket value.", "Retry and check Engine logs if the problem continues.", "bucket_value_upsert", "", "not_committed", "")
			return
		}

		bucketIDStr := chi.URLParam(r, "id")
		// The route owns bucket identity; body and query values cannot override it.
		if bucketIDStr == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "bucket_id_required", "The bucket ID is required in the request path.", "Provide a bucket ID from `fused-cli bucket list`.", "bucket_value_upsert", "", "not_committed", "")
			return
		}
		bucketID, err := uuid.Parse(bucketIDStr)
		// Malformed identities fail before any bucket lookup or mutation.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_id", "The bucket ID is not a valid UUID.", "Use a bucket ID from `fused-cli bucket list`.", "bucket_value_upsert", "", "not_committed", "")
			return
		}
		// Exact bucket lookup is authoritative for workspace membership.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "bucket_value_upsert")
			return
		}

		var payload UpsertBucketValuePayload
		// Invalid JSON must not become a partially populated routing value.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_value_request", "The bucket value request body is invalid.", "Check service_id, key_name, location, and value, then retry.", "bucket_value_upsert", "", "not_committed", "")
			return
		}

		val := store.BucketValue{
			BucketID:  bucketID,
			ServiceID: payload.ServiceID,
			KeyName:   payload.KeyName,
			Location:  payload.Location,
			Value:     payload.Value,
		}
		// Shared validation prevents unsafe host or protected-header overrides.
		if err := connectionprofile.ValidateLiteralBucketValue(val.Location, val.KeyName, val.Value); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "rejected")...)
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_value", err.Error(), "Use a supported location and a non-protected key name.", "bucket_value_upsert", "", "not_committed", "")
			return
		}

		// Unclassified persistence failures hide store details and retain an unknown commit outcome.
		if err := s.UpsertBucketValue(ctx, val); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "error")...)
			slog.ErrorContext(ctx, "failed to upsert bucket value", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_value_save_failed", "The Engine could not save the bucket value.", "Inspect the current bucket value state before retrying, and use the request or trace ID to check Engine logs.", "bucket_value_upsert", "", "unknown", "")
			return
		}
		span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "updated")...)

		w.WriteHeader(http.StatusNoContent)
	}
}

// DeleteBucketValueHandler deletes one exact service/key value from the bucket
// identified by the route, matching the CLI request contract.
func DeleteBucketValueHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.bucketvalues.delete")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		// Bucket-value deletion requires an authenticated control-plane actor.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to delete a bucket value.", "Log in or provide a valid Fused credential.", "bucket_value_delete", "", "not_committed", "")
			return
		}

		// Workspace verification prevents deletion through a foreign identity.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for this bucket value.", "Retry and check Engine logs if the problem continues.", "bucket_value_delete", "", "not_committed", "")
			return
		}

		bucketIDStr := chi.URLParam(r, "id")
		serviceIDStr := r.URL.Query().Get("service_id")
		keyName := r.URL.Query().Get("key_name")

		// All three identifiers are required to make deletion exact and idempotent.
		if bucketIDStr == "" || serviceIDStr == "" || keyName == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "bucket_value_identity_required", "The bucket ID, service ID, and key name are required.", "Provide the bucket path plus service_id and key_name query values.", "bucket_value_delete", "", "not_committed", "")
			return
		}

		bucketID, err := uuid.Parse(bucketIDStr)
		// Reject malformed bucket identity before any workspace lookup.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_id", "The bucket ID is not a valid UUID.", "Use a bucket ID from `fused-cli bucket list`.", "bucket_value_delete", "", "not_committed", "")
			return
		}
		// Exact bucket lookup is authoritative for workspace membership.
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketMutationLookupError(ctx, w, err, "bucket_value_delete")
			return
		}

		serviceID, err := uuid.Parse(serviceIDStr)
		// Reject malformed service identity before the deletion reaches storage.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_service_id", "The service ID is not a valid UUID.", "Use the service ID shown by the bucket or service commands.", "bucket_value_delete", "", "not_committed", "")
			return
		}

		// Unclassified store failures remain generic and retain an unknown deletion outcome.
		if err := s.DeleteBucketValue(ctx, bucketID, serviceID, keyName); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, serviceID, "", "error")...)
			slog.ErrorContext(ctx, "failed to delete bucket value", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_value_delete_failed", "The Engine could not delete the bucket value.", "Inspect the current bucket value state before retrying, and use the request or trace ID to check Engine logs.", "bucket_value_delete", "", "unknown", "")
			return
		}
		span.SetAttributes(bucketValueWriteAttrs(bucketID, serviceID, "", "deleted")...)

		w.WriteHeader(http.StatusNoContent)
	}
}

func bucketValueWriteAttrs(bucketID, serviceID uuid.UUID, location, outcome string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("bucket_id", bucketID.String()),
		attribute.String("service_id", serviceID.String()),
		attribute.String("outcome", outcome),
	}
	if location != "" {
		attrs = append(attrs, attribute.String("target_location", location))
	}
	return attrs
}
