package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"log/slog"
)

type CreateBucketPayload struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

func CreateBucketHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.buckets.create")
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

		var payload CreateBucketPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		bucket, err := s.CreateBucket(ctx, payload.Name, payload.IsDefault)
		if err != nil {
			if strings.Contains(err.Error(), "uq_workspace_buckets") || strings.Contains(err.Error(), "duplicate key value") {
				http.Error(w, "bucket already exists", http.StatusConflict)
				return
			}
			slog.ErrorContext(ctx, "failed to create bucket", slog.Any("error", err))
			http.Error(w, "failed to create bucket", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bucket)
	}
}

func DeleteBucketHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.buckets.delete")
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

		name := chi.URLParam(r, "name")
		if name == "" {
			http.Error(w, "missing bucket name", http.StatusBadRequest)
			return
		}

		bucket, err := s.GetBucketByName(ctx, name)
		if err != nil {
			slog.ErrorContext(ctx, "failed to resolve bucket before delete", slog.Any("error", err))
			http.Error(w, "failed to resolve bucket", http.StatusInternalServerError)
			return
		}
		span.SetAttributes(attribute.String("bucket_id", bucket.ID.String()))
		summary, err := s.GetBucketConnectSummary(ctx, bucket.ID)
		if err != nil {
			slog.ErrorContext(ctx, "failed to inspect bucket connect usage", slog.Any("error", err))
			http.Error(w, "failed to inspect bucket usage", http.StatusInternalServerError)
			return
		}
		if summary != nil {
			span.SetAttributes(attribute.Int("connected_user_count", summary.ConnectedUserCount))
		}

		if err := s.DeleteBucket(ctx, name); err != nil {
			slog.ErrorContext(ctx, "failed to delete bucket", slog.Any("error", err))
			http.Error(w, "failed to delete bucket", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
