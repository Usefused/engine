package api

import (
	"context"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

// WebhookDiagnosticsHandler keeps explicit message revelation on the authenticated management plane.
func WebhookDiagnosticsHandler(s store.Store, key []byte) http.HandlerFunc {
	// Every reveal rechecks current authorization before touching encrypted storage.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.webhook.diagnostics.read")
		defer span.End()
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		scope, err := authorizedWebhookDiagnosticScope(ctx, r)
		// Runtime tokens and missing or differently scoped grants cannot probe receipt existence.
		if err != nil {
			span.SetStatus(codes.Error, "access_denied")
			writeSDKConfigError(w, err)
			return
		}
		repository, ok := s.(store.WebhookDiagnosticsStore)
		// No alternate path may reconstruct bodies from bus messages or ordinary Activity.
		if !ok {
			http.Error(w, "diagnostics unavailable", http.StatusServiceUnavailable)
			return
		}
		detail, err := repository.GetWebhookDiagnostics(ctx, scope, key)
		// Historical absence, expiry, and foreign registration IDs disclose no private values.
		if err != nil {
			span.SetStatus(codes.Error, "diagnostics_unavailable")
			http.Error(w, "message was not captured or is no longer available", http.StatusNotFound)
			return
		}
		span.SetAttributes(attribute.String("service.id", scope.ServiceID.String()), attribute.String("webhook.id", scope.RegistrationID.String()))
		span.SetStatus(codes.Ok, "read")
		writeJSON(w, detail)
	}
}

// authorizedWebhookDiagnosticScope validates exact identifiers and all independent metadata/body permissions.
func authorizedWebhookDiagnosticScope(ctx context.Context, r *http.Request) (store.WebhookDiagnosticScope, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	// App runtime credentials must never acquire management-plane diagnostic access.
	if !ok || actor.AccountID == uuid.Nil || actor.SubjectID == uuid.Nil || actor.Kind == accesscontrol.SubjectApp {
		return store.WebhookDiagnosticScope{}, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "authenticated management actor required"}
	}
	scope := store.WebhookDiagnosticScope{AccountID: actor.AccountID}
	for _, field := range []struct {
		name   string
		target *uuid.UUID
	}{{"service_id", &scope.ServiceID}, {"registration_id", &scope.RegistrationID}, {"event_id", &scope.EventID}} {
		id, err := uuid.Parse(chi.URLParam(r, field.name))
		// Nil UUIDs cannot weaken the exact SQL lookup, even for a workspace Owner.
		if err != nil || id == uuid.Nil {
			return scope, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "exact service, registration and receipt IDs are required"}
		}
		*field.target = id
	}
	service := accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: scope.ServiceID}
	err := (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor,
		accesscontrol.Requirement{Permission: accesscontrol.PermissionServiceRead, Resource: service},
		accesscontrol.Requirement{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}},
		accesscontrol.Requirement{Permission: accesscontrol.PermissionWebhookDiagnosticsRead, Resource: service})
	return scope, err
}
