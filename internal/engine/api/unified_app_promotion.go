package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// UnifiedAppTrafficHandler shares traffic discovery and explicit promotion between UI and CLI.
func UnifiedAppTrafficHandler(s store.Store) http.HandlerFunc {
	// Every request rechecks actor authority before reading or changing the family target.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.unified_app.traffic")
		defer span.End()
		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		// Exact workspace ownership precedes family permission checks.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		permission := accesscontrol.PermissionAppUnifiedAppRead
		// Promotion changes runtime behavior and requires family management authority.
		if r.Method == http.MethodPost {
			permission = accesscontrol.PermissionAppUnifiedAppManage
		}
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}})
		// An authenticated reader must never gain write access through this shared route.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		repository, ok := s.(store.UnifiedAppPromotionStore)
		// Older storage adapters fail closed instead of acknowledging an in-memory change.
		if !ok {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: 503, message: "Unified App traffic control unavailable"})
			return
		}
		target, err := repository.UnifiedAppTrafficTarget(ctx, app.AppFamilyID)
		// The store also rejects non-Unified App families and archived identities.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		// Reads never activate a version, including a family with no current target.
		if r.Method == http.MethodPost {
			expected, decodeErr := decodeUnifiedAppPromotion(w, r)
			if decodeErr != nil {
				writeSDKConfigError(w, decodeErr)
				return
			}
			err = repository.PromoteUnifiedAppVersion(ctx, app.AppFamilyID, app.AppID, expected)
			// Readiness and concurrent traffic changes are reviewable conflicts, not successful promotions.
			if err != nil {
				writeAppLifecycleError(w, span, unifiedAppPromotionError(err))
				return
			}
			// Close live transport sessions after the durable pointer and session rows commit.
			if target != uuid.Nil && target != app.AppID {
				sandbox.TerminateMCPSessionsForApp(target.String())
			}
			target = app.AppID
		}
		span.SetAttributes(attribute.String("app.id", app.AppID.String()), attribute.String("app.family_id", app.AppFamilyID.String()), attribute.String("outcome", "succeeded"))
		writeJSON(w, map[string]string{"app_family_id": app.AppFamilyID.String(), "active_app_id": optionalUUIDString(target)})
	}
}

// decodeUnifiedAppPromotion requires the exact target observed by the caller before changing traffic.
func decodeUnifiedAppPromotion(w http.ResponseWriter, r *http.Request) (uuid.UUID, error) {
	var input struct {
		Expected *string `json:"expected_active_app_id"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	// Missing state must not turn a stale request into an unconditional overwrite.
	if err := decoder.Decode(&input); err != nil || input.Expected == nil {
		return uuid.Nil, workspaceConfigHTTPError{status: 400, message: "expected_active_app_id is required"}
	}
	var extra any
	// Exactly one document is accepted at this mutation boundary.
	if err := decoder.Decode(&extra); err != io.EOF {
		return uuid.Nil, workspaceConfigHTTPError{status: 400, message: "invalid promotion request"}
	}
	// Empty explicitly represents a family whose serving version was deleted.
	if *input.Expected == "" {
		return uuid.Nil, nil
	}
	expected, err := uuid.Parse(*input.Expected)
	// A malformed previous identity cannot authorize changing the deployment.
	if err != nil {
		return uuid.Nil, workspaceConfigHTTPError{status: 400, message: "expected_active_app_id must be a UUID or empty"}
	}
	return expected, nil
}

// unifiedAppPromotionError keeps deployment conflicts distinct from infrastructure failures.
func unifiedAppPromotionError(err error) error {
	// A competing promotion must be reviewed before another mutation is attempted.
	if errors.Is(err, store.ErrUnifiedAppTrafficChanged) {
		return workspaceConfigHTTPError{status: 409, message: err.Error()}
	}
	// A deleted or corrupt retained bundle cannot receive new traffic.
	if errors.Is(err, store.ErrUnifiedAppBundleNotFound) || errors.Is(err, store.ErrUnifiedAppBundleDigestMismatch) {
		return workspaceConfigHTTPError{status: 409, message: "version is not ready to receive traffic"}
	}
	return err
}
