package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type sdkGenerationStatusResponse struct {
	Status      string    `json:"status"`
	AppFamilyID uuid.UUID `json:"app_family_id"`
	AppID       uuid.UUID `json:"app_id"`
	JobID       string    `json:"job_id,omitempty"`
}

// SDKConfigGenerationHandler reports locally persisted progress while the Engine finalizer remains the sole lifecycle transition owner.
func SDKConfigGenerationHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		// The route is control-plane only even when mounted without the production middleware in a test.
		if !ok {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, "generation_status", "", "not_committed"), ctx)
			return
		}
		appID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "app_id")))
		// Malformed identifiers must fail before any app or Registry lookup.
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid app_id"}, "generation_status", "", "not_committed"), ctx)
			return
		}
		response, err := resolveSDKGenerationStatus(ctx, s, actor.AccountID, appID)
		// This endpoint performs no Registry call or lifecycle mutation, so failures are always read-only.
		if err != nil {
			writeSDKConfigError(w, withWorkspaceConfigErrorMetadata(err, "generation_status", "", "not_committed"), ctx)
			return
		}
		writeJSON(w, response)
	}
}

// resolveSDKGenerationStatus projects one exact SDK version from Engine-owned local state without competing with the background finalizer.
func resolveSDKGenerationStatus(ctx context.Context, s store.Store, accountID, appID uuid.UUID) (sdkGenerationStatusResponse, error) {
	app, err := s.GetApp(ctx, appID)
	// An absent app must not reveal generation identity or family membership.
	if err != nil || app == nil {
		return sdkGenerationStatusResponse{}, workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk version not found"}
	}
	// Cross-workspace identity is indistinguishable from absence at the public boundary.
	if app.AccountID != accountID {
		return sdkGenerationStatusResponse{}, workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk version not found"}
	}
	family, err := s.GetAppFamily(ctx, app.AppFamilyID)
	// Missing family identity makes the app unsuitable for any SDK lifecycle projection.
	if err != nil || family == nil {
		return sdkGenerationStatusResponse{}, workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk version not found"}
	}
	// Adapter and workspace identity are family-owned and must match the exact app read.
	if family.AccountID != accountID || family.Kind != store.AppKindSDK {
		return sdkGenerationStatusResponse{}, workspaceConfigHTTPError{status: http.StatusNotFound, message: "sdk version not found"}
	}
	return projectSDKGenerationStatus(app)
}

// projectSDKGenerationStatus converts one authorized local app row into the bounded polling contract.
func projectSDKGenerationStatus(app *store.App) (sdkGenerationStatusResponse, error) {
	response := sdkGenerationStatusResponse{Status: app.SDKGenerationStatus, AppFamilyID: app.AppFamilyID, AppID: app.AppID, JobID: app.SDKGenerationJobID}
	// Existing active SDKs created before durable generation tracking are complete by lifecycle proof.
	if app.Status.Runnable() {
		// Legacy runnable rows predate explicit generation status but already prove package admission.
		if response.Status == "" {
			response.Status = models.SDKGenerationStatusComplete
		}
		return response, nil
	}
	// Building rows expose only the status durably written by apply or the background finalizer.
	if app.Status == store.AppStatusBuilding && (response.Status == models.SDKGenerationStatusPending || response.Status == models.SDKGenerationStatusFailed) {
		return response, nil
	}
	return sdkGenerationStatusResponse{}, workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk generation state is invalid"}
}
