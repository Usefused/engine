package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/store"
)

// UpdatePlanActionsRequest represents the JSON payload to replace actions.
type UpdatePlanActionsRequest struct {
	Actions json.RawMessage `json:"actions"`
}

// ConfigPlanActionsHandler handles PATCH /config/plans/{planId}/actions.
func ConfigPlanActionsHandler(configStore store.ConfigRepository, s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.config_plan.update_actions")
		defer span.End()

		accountID, err := resolveWorkspaceActor(ctx)
		// Actor resolution precedes plan identity parsing and every state mutation.
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			writeConfigPlanActionsError(w, ctx, http.StatusUnauthorized, "authentication_required", "Authentication is required to update plan actions.", "Log in or provide a valid Fused credential.", "")
			return
		}
		span.SetAttributes(attribute.String("account_id", accountID.String()))

		planIDStr := chi.URLParam(r, "planId")
		planID, err := uuid.Parse(planIDStr)
		// A malformed plan identity cannot select or mutate a persisted plan.
		if err != nil {
			writeConfigPlanActionsError(w, ctx, http.StatusBadRequest, "invalid_plan_id", "The plan ID is not a valid UUID.", "Use the plan ID returned by the corresponding plan command.", "")
			return
		}
		span.SetAttributes(attribute.String("plan_id", planID.String()))

		var req UpdatePlanActionsRequest
		// Invalid JSON is rejected before required-permission recomputation.
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeConfigPlanActionsError(w, ctx, http.StatusBadRequest, "invalid_plan_actions_request", "The plan actions request body is invalid.", "Send a JSON object containing the reviewed actions array.", planID.String())
			return
		}

		// An omitted action set means the caller reviewed and selected no actions.
		if len(req.Actions) == 0 {
			req.Actions = []byte("[]")
		}

		requiredPermissions, requiredCount, err := requiredPermissionsFromContext(ctx, req.Actions)
		// Permission snapshot failures are internal and must not expose authorization internals.
		if err != nil {
			writeConfigPlanActionsError(w, ctx, http.StatusInternalServerError, "plan_permission_snapshot_unavailable", "The Engine could not verify the plan permission snapshot.", "Retry and check Engine logs if the problem continues.", planID.String())
			return
		}

		plan, err := configStore.ReplaceConfigPlanActions(ctx, planID, req.Actions, requiredPermissions, accountID)
		// Store sentinels retain stable conflict/not-found semantics; unknown failures
		// remain private and receive only fixed internal copy.
		if err != nil {
			if errors.Is(err, store.ErrConfigPlanApplyInProgress) {
				writeConfigPlanActionsError(w, ctx, http.StatusConflict, "plan_apply_in_progress", "Plan apply is already in progress.", "Wait for the active apply to finish before changing plan actions.", planID.String())
				return
			}
			if errors.Is(err, store.ErrConfigPlanNotFound) {
				writeConfigPlanActionsError(w, ctx, http.StatusNotFound, "config_plan_not_pending", "The plan was not found or is no longer pending.", "Create a fresh plan before changing its actions.", planID.String())
				return
			}
			slog.ErrorContext(ctx, "ConfigPlanActionsHandler: ReplaceConfigPlanActions error", slog.Any("error", err))
			writeConfigPlanActionsError(w, ctx, http.StatusInternalServerError, "plan_actions_update_failed", "The Engine could not update plan actions.", "Retry and check Engine logs if the problem continues.", planID.String())
			return
		}

		span.SetAttributes(
			attribute.Int("required_permissions_count", requiredCount),
			attribute.String("outcome", "success"),
		)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":               "updated",
			"plan_id":              plan.ID.String(),
			"revision":             plan.Revision,
			"required_permissions": plan.RequiredPermissions,
		})
	}
}

// writeConfigPlanActionsError records the exact pre-commit plan mutation phase
// and emits one correlated structured control-plane failure.
func writeConfigPlanActionsError(w http.ResponseWriter, ctx context.Context, status int, code, message, remediation, planID string) {
	writeWorkspaceConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{
		status: status, code: code, message: message, remediation: remediation,
	}, "plan_action_update", planID, "not_committed"), ctx)
}
