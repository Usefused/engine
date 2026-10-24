package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

// UnifiedAppRecoveryControlHandler lets app managers recover cron or webhook runs without a caller read handle.
func UnifiedAppRecoveryControlHandler(s store.Store) http.HandlerFunc {
	// Every management request resolves its persisted app family before reading history or admitting effects.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.unified_app.recovery.control")
		defer span.End()
		w.Header().Set("Cache-Control", "no-store")
		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		// Runtime tokens and foreign accounts cannot become management authority.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		permission := accesscontrol.PermissionAppUnifiedAppManage
		// Attempt metadata is ordinary app history; triggering provider-capable work requires management.
		if r.Method == http.MethodGet {
			permission = accesscontrol.PermissionAppUnifiedAppRead
		}
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}})
		// Failed authorization stops before private input or recovery state is read.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		serveRecoveryControl(ctx, w, r, s, app)
	}
}

// serveRecoveryControl keeps tenant-scoped result lookup shared between history reads and manual triggers.
func serveRecoveryControl(ctx context.Context, w http.ResponseWriter, r *http.Request, s store.Store, app *store.App) {
	id, err := uuid.Parse(chi.URLParam(r, "execution_id"))
	// Invalid IDs cannot become unscoped selectors.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(404, "execution_not_found", "execution not found"))
		return
	}
	repository, ok := s.(store.ExecutionRecoveryStore)
	results, resultsOK := s.(store.ExecutionResultStore)
	// Management cannot manufacture durability when a deployment lacks recovery storage.
	if !ok || !resultsOK {
		writeRESTExecutionError(w, newRESTExecutionError(503, "runtime_unavailable", "execution recovery is unavailable"))
		return
	}
	record, err := results.GetExecutionResult(ctx, app.AccountID, app.AppID, id)
	// Expired and cross-account executions remain indistinguishable even to app managers.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(404, "execution_not_found", "execution not found"))
		return
	}
	// Reads expose bounded metadata only, never decrypted continuation payloads.
	if r.Method == http.MethodGet {
		attempts, err := repository.ListExecutionAttempts(ctx, app.AccountID, app.AppID, id)
		if err != nil {
			writeRESTExecutionError(w, newRESTExecutionError(503, "result_unavailable", "execution recovery is unavailable"))
			return
		} // A database failure is not an empty history.
		writeJSON(w, map[string]any{"attempts": attempts})
		return
	}
	enqueueControlRecovery(ctx, w, r, repository, *record)
}

// enqueueControlRecovery durably reserves manual work for the existing scheduler instead of creating another runtime.
func enqueueControlRecovery(ctx context.Context, w http.ResponseWriter, r *http.Request, repository store.ExecutionRecoveryStore, source store.ExecutionResult) {
	key, requestErr := rerunIdempotencyKeyHash(r)
	// Repeated control submissions must identify one accepted attempt.
	if requestErr != nil {
		writeRESTExecutionError(w, requestErr)
		return
	}
	_, err := repository.AuthorizeExecutionRecovery(ctx, source.AccountID, source.AppID, source.ID)
	// Management does not resurrect revoked provider execution authority or retired app versions.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(409, "recovery_conflict", "original execution authority is unavailable"))
		return
	}
	next, _, err := repository.ReserveExecutionAttempt(ctx, store.ExecutionRerunRequest{AccountID: source.AccountID, AppID: source.AppID, SourceID: source.ID, TokenID: source.AppTokenID, KeyHash: key, Enqueue: true})
	// Admission failures never imply that a background worker will execute the request.
	if errors.Is(err, store.ErrExecutionRecoveryConflict) {
		writeRESTExecutionError(w, newRESTExecutionError(409, "recovery_conflict", "execution cannot be rerun in its current state"))
		return
	}
	// Unknown persistence outcomes require a retry with the same key, never a guessed conflict.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(503, "result_unavailable", "execution recovery is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// A newly queued attempt is durable before the management caller receives acceptance.
	if next.Status == "queued" || next.Status == "running" {
		w.WriteHeader(http.StatusAccepted)
	}
	writeJSON(w, projectCapabilityExecution(next, ""))
}
