package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/store"
)

// isIntegrationDeletePath reports whether r is a top-level service deletion (DELETE /integrations/{id})
// but NOT a sub-resource delete like DELETE /integrations/session/{id} or
// DELETE /integrations/{id}/import-warnings. Those are unrelated Registry operations
// that must flow through without triggering workspace cleanup.
func isIntegrationDeletePath(method, path string) bool {
	if method != http.MethodDelete {
		return false
	}
	// Only match exactly two path segments: /integrations/<uuid>
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	return len(parts) == 2 && parts[0] == "integrations" && parts[1] != ""
}

// forwardIntegrationDeleteWithWorkspaceCleanup forwards the delete request to
// the Registry, and if successful, removes the service from the local Engine
// workspace to keep the sandbox in sync.
func forwardIntegrationDeleteWithWorkspaceCleanup(proxy Forwarder, s store.Store, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.rest_mutation", trace.WithAttributes(
		attribute.String("user_action", "rest."+r.Method),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	rec := newStatusRecorder(w)
	// Extract the service ID now -- the path is guaranteed to be /integrations/<uuid>
	// by isIntegrationDeletePath, so parts[1] is always the service UUID.
	serviceIDStr := strings.TrimPrefix(r.URL.Path, "/integrations/")
	serviceID, parseErr := uuid.Parse(serviceIDStr)

	wsErr := verifyWorkspaceActor(ctx, accountID)

	proxy.ForwardAndInspect(rec, r.WithContext(ctx), "", func(_ *http.Response, body []byte) {
		// Only clean up the workspace if the Registry confirmed the deletion.
		// Skip if we failed to resolve the workspace -- the Registry-side delete
		// already succeeded, so we log and move on rather than blocking.
		if rec.status != http.StatusNoContent && rec.status != http.StatusOK {
			return
		}
		if parseErr != nil || wsErr != nil {
			return
		}
		cleanupDeletedService(ctx, s, serviceID)
	})
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

func cleanupDeletedService(ctx context.Context, s store.Store, serviceID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.cleanup_deleted_service")
	defer span.End()

	// ErrWorkspaceServiceNotFound is fine -- the service may simply not have been
	// activated in this workspace, which is not an error worth surfacing.
	err := s.RemoveWorkspaceService(ctx, serviceID)
	if err != nil && err != store.ErrWorkspaceServiceNotFound {
		span.RecordError(err)
		span.SetAttributes(attribute.String("outcome", "error"))
		return
	}
	span.SetAttributes(attribute.String("outcome", "success"))
}
