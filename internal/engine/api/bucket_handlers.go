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

// CreateBucketHandler creates one workspace bucket and returns stable control
// diagnostics for authorization, quota, validation, and persistence failures.
func CreateBucketHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.buckets.create")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		// Bucket creation requires an authenticated control-plane actor.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to create a bucket.", "Log in or provide a valid Fused credential.", "bucket_create", "", "not_committed", "")
			return
		}

		// Resolve the actor's exact workspace before applying its bucket quota.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for bucket creation.", "Retry and check Engine logs if the problem continues.", "bucket_create", "", "not_committed", "")
			return
		}

		var payload CreateBucketPayload
		// Invalid JSON cannot safely define bucket identity or default state.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "invalid_bucket_request", "The bucket request body is invalid.", "Provide a non-empty bucket name and retry.", "bucket_create", "", "not_committed", "")
			return
		}

		currentBuckets, err := s.CountBuckets(ctx)
		// Quota evaluation must fail closed if current usage is unavailable.
		if err != nil {
			slog.ErrorContext(ctx, "failed to count buckets", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_count_failed", "The Engine could not inspect current bucket usage.", "Retry and check Engine logs if the problem continues.", "bucket_create", "", "not_committed", "")
			return
		}
		// A reached entitlement is an explicit quota diagnosis, not a generic denial.
		if limErr := entitlement.CheckLimit(span, "buckets", currentBuckets, entitlement.LiveEntitlement.Load().MaxBuckets); limErr != nil {
			span.SetAttributes(attribute.String("outcome", "limit_exceeded"))
			writeControlAPIMutationError(w, ctx, http.StatusForbidden, "bucket_limit_exceeded", "The workspace bucket limit has been reached.", "Delete an unused bucket or change the workspace plan before retrying.", "bucket_create", "", "not_committed", "")
			return
		}

		bucket, err := s.CreateBucket(ctx, payload.Name, payload.IsDefault)
		// Duplicate names are a caller-resolvable conflict; other store failures stay opaque.
		if err != nil {
			// Store constraint names are inspected only to classify the known conflict.
			if strings.Contains(err.Error(), "uq_workspace_buckets") || strings.Contains(err.Error(), "duplicate key value") {
				writeControlAPIMutationError(w, ctx, http.StatusConflict, "bucket_already_exists", "A bucket with this name already exists.", "Choose a different bucket name or use the existing bucket.", "bucket_create", "", "not_committed", "")
				return
			}
			slog.ErrorContext(ctx, "failed to create bucket", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_create_failed", "The Engine could not create the bucket.", "Inspect the current bucket list before retrying, and use the request or trace ID to check Engine logs.", "bucket_create", "", "unknown", "")
			return
		}
		span.SetAttributes(attribute.String("outcome", "created"))

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(bucket)
	}
}

// DeleteBucketHandler deletes only the bucket identity authorized by middleware
// and preserves actionable conflict and not-found diagnostics.
func DeleteBucketHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.api.buckets.delete")
		defer span.End()

		accountID, err := controlActorAccount(ctx)
		// Bucket deletion requires an authenticated control-plane actor.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to delete a bucket.", "Log in or provide a valid Fused credential.", "bucket_delete", "", "not_committed", "")
			return
		}

		// Workspace resolution keeps authorization scoped to the actor's workspace.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "workspace_resolution_failed", "The Engine could not resolve the workspace for bucket deletion.", "Retry and check Engine logs if the problem continues.", "bucket_delete", "", "not_committed", "")
			return
		}

		name := chi.URLParam(r, "name")
		// A missing route name cannot identify a deletion target.
		if name == "" {
			writeControlAPIMutationError(w, ctx, http.StatusBadRequest, "bucket_name_required", "The bucket name is required in the request path.", "Choose a bucket from `fused-cli bucket list`.", "bucket_delete", "", "not_committed", "")
			return
		}

		bucketID, err := authorizedBucketDeleteID(ctx)
		// Middleware's exact bucket ID is authoritative across name replacement races.
		if err != nil {
			writeControlAPIMutationError(w, ctx, http.StatusForbidden, "bucket_delete_permission_denied", "The authorized bucket identity is unavailable.", "Refresh bucket state and verify bucket.manage permission.", "bucket_delete", "", "not_committed", "")
			return
		}
		span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
		summary, err := s.GetBucketConnectSummary(ctx, bucketID)
		// Usage inspection must succeed before the confirmed destructive mutation.
		if err != nil {
			slog.ErrorContext(ctx, "failed to inspect bucket connect usage", slog.Any("error", err))
			writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_usage_lookup_failed", "The Engine could not inspect bucket usage before deletion.", "Retry and check Engine logs if the problem continues.", "bucket_delete", "", "not_committed", "")
			return
		}
		// Connected-user count is telemetry only and never changes deletion policy.
		if summary != nil {
			span.SetAttributes(attribute.Int("connected_user_count", summary.ConnectedUserCount))
		}

		if err := s.DeleteBucket(ctx, name, bucketID); err != nil {
			writeDeleteBucketError(ctx, w, span, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", "deleted"))

		w.WriteHeader(http.StatusNoContent)
	}
}

// authorizedBucketDeleteID returns the exact resource identity admitted by
// authorization middleware instead of resolving a mutable name again.
func authorizedBucketDeleteID(ctx context.Context) (uuid.UUID, error) {
	requirements, ok := accesscontrol.RequiredPermissionsFromContext(ctx)
	// Missing middleware evidence is an authorization denial, not a lookup hint.
	if !ok {
		return uuid.Nil, accesscontrol.ErrPolicyDenied
	}
	for _, requirement := range requirements {
		// Only an exact non-zero bucket.manage requirement authorizes deletion.
		if requirement.Permission == accesscontrol.PermissionBucketManage && requirement.Resource.Type == accesscontrol.ResourceBucket && requirement.Resource.ID != uuid.Nil {
			return requirement.Resource.ID, nil
		}
	}
	return uuid.Nil, accesscontrol.ErrPolicyDenied
}

// writeDeleteBucketError maps known store outcomes to stable, structured
// control-plane errors and hides all unknown persistence detail.
func writeDeleteBucketError(ctx context.Context, w http.ResponseWriter, span trace.Span, err error) {
	// Known domain sentinels are safe and distinguish conflict from absence.
	switch {
	case errors.Is(err, store.ErrBucketBound):
		span.SetAttributes(attribute.String("outcome", "conflict"))
		writeControlAPIMutationError(w, ctx, http.StatusConflict, "bucket_bound", "The bucket is bound to an app and cannot be deleted.", "Move the dependent app bindings before deleting this bucket.", "bucket_delete", "", "not_committed", "")
	case errors.Is(err, store.ErrDefaultBucketProtected):
		span.SetAttributes(attribute.String("outcome", "conflict"))
		writeControlAPIMutationError(w, ctx, http.StatusConflict, "default_bucket_protected", "The default bucket cannot be deleted.", "Choose another default bucket before retrying.", "bucket_delete", "", "not_committed", "")
	case errors.Is(err, store.ErrBucketNotFound):
		writeControlAPIMutationError(w, ctx, http.StatusNotFound, "bucket_not_found", "The selected bucket was not found.", "Refresh the bucket list before retrying.", "bucket_delete", "", "not_committed", "")
	default:
		slog.ErrorContext(ctx, "failed to delete bucket", slog.Any("error", err))
		writeControlAPIMutationError(w, ctx, http.StatusInternalServerError, "bucket_delete_failed", "The Engine could not delete the bucket.", "Inspect the current bucket list before retrying, and use the request or trace ID to check Engine logs.", "bucket_delete", "", "unknown", "")
	}
}
