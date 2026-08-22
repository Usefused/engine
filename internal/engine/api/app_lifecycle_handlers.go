package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/applifecycle"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

type appDeprecationRequest struct {
	Message                 string `json:"message"`
	PlannedDeactivationDate string `json:"planned_deactivation_at,omitempty"`
}

type appLifecycleResponse struct {
	Status      string `json:"status"`
	AppFamilyID string `json:"app_family_id"`
	AppID       string `json:"app_id"`
}

func DeprecateAppHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.app.deprecate")
		defer span.End()

		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		request, plannedAt, err := decodeAppDeprecationRequest(r)
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}

		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
		if err := applifecycle.New(s).Deprecate(ctx, app.AppID, request.Message, plannedAt); err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", "deprecated"))
		writeJSON(w, appLifecycleResponse{Status: "deprecated", AppFamilyID: app.AppFamilyID.String(), AppID: app.AppID.String()})
	}
}

func UndeprecateAppHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.app.undeprecate")
		defer span.End()

		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
		if err := applifecycle.New(s).Undeprecate(ctx, app.AppID); err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		span.SetAttributes(attribute.String("outcome", "active"))
		writeJSON(w, appLifecycleResponse{Status: "active", AppFamilyID: app.AppFamilyID.String(), AppID: app.AppID.String()})
	}
}

func DeactivateAppHandler(s store.Store, proxy Forwarder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.app.deactivate")
		defer span.End()

		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		span.SetAttributes(attribute.String("actor.type", string(actor.Kind)))
		if err := applifecycle.New(s).Deactivate(ctx, app.AppID, actor.SubjectID); err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}

		cleanupDeactivatedAppRuntime(ctx, app, proxy)
		span.SetAttributes(attribute.String("outcome", "deactivated"))
		writeJSON(w, appLifecycleResponse{Status: "deactivated", AppFamilyID: app.AppFamilyID.String(), AppID: app.AppID.String()})
	}
}

func lifecycleActorAndApp(ctx context.Context, s store.Store, r *http.Request) (accesscontrol.Actor, *store.App, error) {
	actor, ok := accesscontrol.ActorFromContext(ctx)
	if !ok || actor.AccountID == uuid.Nil || actor.SubjectID == uuid.Nil {
		return accesscontrol.Actor{}, nil, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "authenticated actor required"}
	}
	appID, err := uuid.Parse(chi.URLParam(r, "app_id"))
	if err != nil {
		return accesscontrol.Actor{}, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "app_id must be a UUID"}
	}
	app, err := appOwnedBy(ctx, s, actor.AccountID, appID)
	if err != nil {
		return accesscontrol.Actor{}, nil, err
	}
	return actor, app, nil
}

func appOwnedBy(ctx context.Context, s store.Store, accountID, appID uuid.UUID) (*store.App, error) {
	app, err := s.GetApp(ctx, appID)
	if errors.Is(err, store.ErrAppNotFound) {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "app not found"}
	}
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load app"}
	}
	if app.AccountID != accountID {
		return nil, workspaceConfigHTTPError{status: http.StatusForbidden, message: "app is not available in this workspace"}
	}
	return app, nil
}

func decodeAppDeprecationRequest(r *http.Request) (appDeprecationRequest, *time.Time, error) {
	var request appDeprecationRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid deprecation request"}
	}
	request.Message = strings.TrimSpace(request.Message)
	if request.Message == "" {
		return request, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "message is required"}
	}
	if strings.TrimSpace(request.PlannedDeactivationDate) == "" {
		return request, nil, nil
	}
	plannedAt, err := time.Parse(time.RFC3339, request.PlannedDeactivationDate)
	if err != nil {
		return request, nil, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "planned_deactivation_at must be RFC3339"}
	}
	return request, &plannedAt, nil
}

func cleanupDeactivatedAppRuntime(ctx context.Context, app *store.App, proxy Forwarder) {
	if app == nil {
		return
	}
	if app.GeneratorVersion != "" {
		if err := deleteRegistryPackage(ctx, proxy, app.AppID); err != nil {
			// Engine authorization is already permanently denied. Package deletion
			// is external cleanup and must never roll the executable state back.
			slog.WarnContext(ctx, "app package cleanup pending", slog.Any("error", err), slog.String("app_id", app.AppID.String()))
		}
		return
	}
	sandbox.TerminateMCPSessionsForApp(app.AppID.String())
}

func deleteRegistryPackage(ctx context.Context, proxy Forwarder, appID uuid.UUID) error {
	if proxy == nil {
		return errors.New("registry package cleanup unavailable")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, "/sdk-packages/"+appID.String(), nil)
	if err != nil {
		return err
	}
	recorder := httptest.NewRecorder()
	proxy.Forward(recorder, request, "")
	if recorder.Code == http.StatusNotFound || recorder.Code >= http.StatusOK && recorder.Code < http.StatusMultipleChoices {
		return nil
	}
	return sdkProxyError{status: recorder.Code, body: recorder.Body.Bytes()}
}

func writeAppLifecycleError(w http.ResponseWriter, span trace.Span, err error) {
	span.SetAttributes(attribute.String("outcome", "failed"))
	if errors.Is(err, store.ErrAppNotFound) {
		writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusNotFound, message: "app not found"})
		return
	}
	writeSDKConfigError(w, err)
}
