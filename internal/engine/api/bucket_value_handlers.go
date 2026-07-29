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

func UpsertBucketValueHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.bucketvalues.upsert")
		defer span.End()

		apiKey := r.Header.Get("X-API-Key")
		accountID, err := validateAPIKey(ctx, s, apiKey)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := s.VerifyWorkspaceOwner(ctx, accountID); err != nil {
			http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
			return
		}

		bucketIDStr := chi.URLParam(r, "id")
		if bucketIDStr == "" {
			http.Error(w, "missing bucket_id", http.StatusBadRequest)
			return
		}
		bucketID, err := uuid.Parse(bucketIDStr)
		if err != nil {
			http.Error(w, "invalid bucket_id format", http.StatusBadRequest)
			return
		}
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketLookupError(w, err)
			return
		}

		var payload UpsertBucketValuePayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		val := store.BucketValue{
			BucketID:  bucketID,
			ServiceID: payload.ServiceID,
			KeyName:   payload.KeyName,
			Location:  payload.Location,
			Value:     payload.Value,
		}
		if err := connectionprofile.ValidateLiteralBucketValue(val.Location, val.KeyName, val.Value); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "rejected")...)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if err := s.UpsertBucketValue(ctx, val); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "error")...)
			slog.ErrorContext(ctx, "failed to upsert bucket value", slog.Any("error", err))
			http.Error(w, "failed to save bucket value", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(bucketValueWriteAttrs(bucketID, payload.ServiceID, payload.Location, "updated")...)

		w.WriteHeader(http.StatusNoContent)
	}
}

func DeleteBucketValueHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.bucketvalues.delete")
		defer span.End()

		apiKey := r.Header.Get("X-API-Key")
		accountID, err := validateAPIKey(ctx, s, apiKey)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := s.VerifyWorkspaceOwner(ctx, accountID); err != nil {
			http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
			return
		}

		bucketIDStr := r.URL.Query().Get("bucket_id")
		serviceIDStr := r.URL.Query().Get("service_id")
		keyName := r.URL.Query().Get("key_name")

		if bucketIDStr == "" || serviceIDStr == "" || keyName == "" {
			http.Error(w, "missing bucket_id, service_id, or key_name", http.StatusBadRequest)
			return
		}

		bucketID, err := uuid.Parse(bucketIDStr)
		if err != nil {
			http.Error(w, "invalid bucket_id", http.StatusBadRequest)
			return
		}
		if err := verifyBucketInWorkspace(ctx, s, bucketID); err != nil {
			writeBucketLookupError(w, err)
			return
		}

		serviceID, err := uuid.Parse(serviceIDStr)
		if err != nil {
			http.Error(w, "invalid service_id", http.StatusBadRequest)
			return
		}

		if err := s.DeleteBucketValue(ctx, bucketID, serviceID, keyName); err != nil {
			span.SetAttributes(bucketValueWriteAttrs(bucketID, serviceID, "", "error")...)
			slog.ErrorContext(ctx, "failed to delete bucket value", slog.Any("error", err))
			http.Error(w, "failed to delete bucket value", http.StatusInternalServerError)
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
