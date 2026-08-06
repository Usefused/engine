package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

var errSDKPackageStreamInterrupted = errors.New("SDK package stream interrupted")

type SDKPackageClient interface {
	DownloadSDKPackage(context.Context, uuid.UUID) (*http.Response, error)
}

func SDKPackageDownloadHandler(s store.Store, proxy Forwarder, packages SDKPackageClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.sdk_package.download")
		defer span.End()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		if !ok {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusUnauthorized, message: "invalid API key or workspace not found"}, ctx)
			return
		}
		appID, err := uuid.Parse(chi.URLParam(r, "app_id"))
		if err != nil {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: http.StatusBadRequest, message: "invalid app_id"}, ctx)
			return
		}
		span.SetAttributes(
			attribute.String("user_action", "sdk.download"),
			attribute.String("actor.type", string(actor.Kind)),
			attribute.String("account_id", actor.AccountID.String()),
			attribute.String("app.id", appID.String()),
		)
		if err := serveSDKPackage(ctx, w, r.Header.Get("X-API-Key"), actor, appID, s, proxy, packages); err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "SDK package download failed")
			if errors.Is(err, errSDKPackageStreamInterrupted) {
				return
			}
			writeSDKConfigError(w, err, ctx)
			return
		}
		span.SetStatus(codes.Ok, "SDK package downloaded")
	}
}

func serveSDKPackage(ctx context.Context, w http.ResponseWriter, apiKey string, actor accesscontrol.Actor, appID uuid.UUID, s store.Store, proxy Forwarder, packages SDKPackageClient) error {
	buildStore, ok := s.(store.SDKPackageBuildStore)
	if !ok || packages == nil {
		return sdkPackageDependencyError("SDK package recovery is unavailable")
	}
	request, err := buildStore.GetSDKPackageBuildRequest(ctx, actor.AccountID, appID)
	if errors.Is(err, store.ErrAppNotFound) {
		return workspaceConfigHTTPError{status: http.StatusNotFound, message: "SDK app version not found"}
	}
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load SDK package definition"}
	}
	response, err := packages.DownloadSDKPackage(ctx, appID)
	if err != nil {
		return sdkPackageDependencyError("The Registry could not load this SDK package")
	}
	if response.StatusCode != http.StatusNotFound {
		return streamSDKPackageResponse(w, response)
	}
	response.Body.Close()
	if err := regenerateSDKPackage(ctx, proxy, apiKey, actor, request); err != nil {
		return err
	}
	retry, err := packages.DownloadSDKPackage(ctx, appID)
	if err != nil {
		return sdkPackageDependencyError("The regenerated SDK package could not be loaded")
	}
	if retry.StatusCode == http.StatusNotFound {
		retry.Body.Close()
		return sdkPackageDependencyError("SDK generation completed without a downloadable package")
	}
	return streamSDKPackageResponse(w, retry)
}

func regenerateSDKPackage(ctx context.Context, proxy Forwarder, apiKey string, actor accesscontrol.Actor, request *models.SDKGenerationRequest) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.sdk_package.cache_regenerate")
	defer span.End()
	span.SetAttributes(
		attribute.String("user_action", "sdk.download.cache_regenerate"),
		attribute.String("actor.type", string(actor.Kind)),
		attribute.String("account_id", actor.AccountID.String()),
		attribute.String("app.id", request.AppID.String()),
		attribute.String("app.family_id", request.AppFamilyID.String()),
		attribute.String("sdk.generator_version", request.GeneratorVersion),
	)
	payload, err := json.Marshal(request)
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to prepare SDK package regeneration"}
	}
	result, err := runTrackedSDKGeneration(ctx, proxy, apiKey, payload)
	if err != nil {
		return safeSDKPackageGenerationError(err)
	}
	if err := validateRegistryAppIdentity(payload, result.AppID); err != nil {
		return err
	}
	result, err = awaitSDKGenerationCompletion(ctx, proxy, apiKey, result)
	if err != nil {
		return safeSDKPackageGenerationError(err)
	}
	if result.Status != models.SDKGenerationStatusComplete {
		return workspaceConfigHTTPError{status: http.StatusConflict, message: "sdk_generation_failed"}
	}
	span.SetAttributes(attribute.String("outcome", "regenerated"))
	return nil
}

func safeSDKPackageGenerationError(err error) error {
	var proxyErr sdkProxyError
	if !errors.As(err, &proxyErr) || proxyErr.status != http.StatusConflict {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(proxyErr.body, &body) == nil && body.Error == "sdk_generator_version_unavailable" {
		return workspaceConfigHTTPError{
			status: http.StatusConflict, code: body.Error,
			message:  "This SDK version uses a generator that is no longer available.",
			category: "dependency", retryable: false,
		}
	}
	return err
}

func streamSDKPackageResponse(w http.ResponseWriter, response *http.Response) error {
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return sdkPackageDependencyError("The Registry could not download this SDK package")
	}
	copySDKPackageHeader(w.Header(), response.Header, "Content-Type")
	copySDKPackageHeader(w.Header(), response.Header, "Content-Disposition")
	copySDKPackageHeader(w.Header(), response.Header, "Content-Length")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, response.Body); err != nil {
		slog.Warn("SDK package stream interrupted", slog.Any("error", err))
		return fmt.Errorf("%w: %v", errSDKPackageStreamInterrupted, err)
	}
	return nil
}

func copySDKPackageHeader(target, source http.Header, key string) {
	if value := strings.TrimSpace(source.Get(key)); value != "" {
		target.Set(key, value)
	}
}

func sdkPackageDependencyError(message string) error {
	return workspaceConfigHTTPError{
		status: http.StatusBadGateway, code: "sdk_package_unavailable", message: message,
		category: "dependency", retryable: true,
	}
}
