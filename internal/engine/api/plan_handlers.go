package api

import (
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

		accountID, err := resolveWorkspaceActor(ctx, s, r)
		if err != nil {
			span.SetAttributes(attribute.String("outcome", "unauthorized"))
			http.Error(w, `{"error":"invalid API key or workspace not found"}`, http.StatusUnauthorized)
			return
		}
		span.SetAttributes(attribute.String("account_id", accountID.String()))

		planIDStr := chi.URLParam(r, "planId")
		planID, err := uuid.Parse(planIDStr)
		if err != nil {
			http.Error(w, `{"error":"invalid planId UUID"}`, http.StatusBadRequest)
			return
		}
		span.SetAttributes(attribute.String("plan_id", planID.String()))

		var req UpdatePlanActionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		if len(req.Actions) == 0 {
			req.Actions = []byte("[]")
		}

		plan, err := configStore.ReplaceConfigPlanActions(ctx, planID, req.Actions, uuid.Nil)
		if err != nil {
			if errors.Is(err, store.ErrConfigPlanNotFound) {
				http.Error(w, `{"error":"plan not found or not pending"}`, http.StatusNotFound)
				return
			}
			slog.ErrorContext(ctx, "ConfigPlanActionsHandler: ReplaceConfigPlanActions error", slog.Any("error", err))
			http.Error(w, `{"error":"failed to update plan actions"}`, http.StatusInternalServerError)
			return
		}

		span.SetAttributes(attribute.String("outcome", "success"))

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":   "updated",
			"plan_id":  plan.ID.String(),
			"revision": plan.Revision,
		})
	}
}
