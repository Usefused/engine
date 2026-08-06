package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type CreateBucketPayload struct {
	Name      string `json:"name"`
	IsDefault bool   `json:"is_default"`
}

func CreateBucketHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.buckets.create")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
			return
		}

		var payload CreateBucketPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "invalid request body", http.StatusBadRequest)
			return
		}

		currentBuckets, err := s.CountBuckets(ctx)
		if err != nil {
			slog.ErrorContext(ctx, "failed to count buckets", slog.Any("error", err))
			http.Error(w, "failed to count buckets", http.StatusInternalServerError)
			return
		}
		if limErr := entitlement.CheckLimit(span, "buckets", currentBuckets, entitlement.LiveEntitlement.Load().MaxBuckets); limErr != nil {
			span.SetAttributes(attribute.String("outcome", "limit_exceeded"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": limErr.Error()})
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

		accountID, err := controlActorAccount(ctx)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			http.Error(w, "failed to resolve workspace", http.StatusInternalServerError)
			return
		}

		name := chi.URLParam(r, "name")
		if name == "" {
			http.Error(w, "missing bucket name", http.StatusBadRequest)
			return
		}

		bucketID, err := authorizedBucketDeleteID(ctx)
		if err != nil {
			http.Error(w, "authorized bucket identity unavailable", http.StatusForbidden)
			return
		}
		span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
		summary, err := s.GetBucketConnectSummary(ctx, bucketID)
		if err != nil {
			slog.ErrorContext(ctx, "failed to inspect bucket connect usage", slog.Any("error", err))
			http.Error(w, "failed to inspect bucket usage", http.StatusInternalServerError)
			return
		}
		if summary != nil {
			span.SetAttributes(attribute.Int("connected_user_count", summary.ConnectedUserCount))
		}

		if err := s.DeleteBucket(ctx, name, bucketID); err != nil {
			writeDeleteBucketError(ctx, w, span, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func authorizedBucketDeleteID(ctx context.Context) (uuid.UUID, error) {
	requirements, ok := accesscontrol.RequiredPermissionsFromContext(ctx)
	if !ok {
		return uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	for _, requirement := range requirements {
		if requirement.Permission == accesscontrol.PermissionBucketManage && requirement.Resource.Type == accesscontrol.ResourceBucket && requirement.Resource.ID != uuid.Nil {
			return requirement.Resource.ID, nil
		}
	}
	return uuid.Nil, accesscontrol.ErrPolicyDenied
}

func writeDeleteBucketError(ctx context.Context, w http.ResponseWriter, span trace.Span, err error) {
	switch {
	case errors.Is(err, store.ErrBucketBound), errors.Is(err, store.ErrDefaultBucketProtected):
		span.SetAttributes(attribute.String("outcome", "conflict"))
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrBucketNotFound):
		http.Error(w, "bucket not found", http.StatusNotFound)
	default:
		slog.ErrorContext(ctx, "failed to delete bucket", slog.Any("error", err))
		http.Error(w, "failed to delete bucket", http.StatusInternalServerError)
	}
}
