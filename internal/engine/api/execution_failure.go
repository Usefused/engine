package api

import (
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"net/http"
)

// ExecutionFailureHandler projects only the retained error for ordinary receipt readers, never input, output or stacks.
func ExecutionFailureHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.execution.failure.read")
		defer span.End()
		w.Header().Set("Cache-Control", "no-store")
		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		// Exact version ownership is checked before either authorization or retained-result access.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		ref := accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor,
			accesscontrol.Requirement{Permission: accesscontrol.PermissionAppUnifiedAppRead, Resource: ref},
			accesscontrol.Requirement{Permission: accesscontrol.PermissionAuditRead, Resource: ref})
		// Reading an error requires the same app and audit grants as the execution receipt.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "execution_id"))
		// Invalid IDs never become unbounded storage selectors.
		if err != nil {
			http.Error(w, "invalid execution ID", 400)
			return
		}
		results, ok := s.(store.ExecutionResultStore)
		// Older installations cannot synthesize messages from telemetry or private payloads.
		if !ok {
			http.Error(w, "execution unavailable", 503)
			return
		}
		record, err := results.GetExecutionResult(ctx, actor.AccountID, app.AppID, id)
		// The result store enforces both ownership and retention before returning any message.
		if err != nil {
			http.Error(w, "execution unavailable", 404)
			return
		}
		writeJSON(w, struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{record.ErrorCode, record.ErrorMessage})
	}
}
