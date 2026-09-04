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
	"go.opentelemetry.io/otel/trace"
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
			recordSDKPackageDownloadFailure(span, err)
			if errors.Is(err, errSDKPackageStreamInterrupted) {
				return
			}
			writeSDKConfigError(w, err, ctx)
			return
		}
		span.SetStatus(codes.Ok, "SDK package downloaded")
	}
}

// recordSDKPackageDownloadFailure records only a stable code so Registry,
// storage, URL, and stream errors cannot leak into OTEL exception fields.
func recordSDKPackageDownloadFailure(span trace.Span, err error) {
	code := "sdk_package_download_failed"
	// Stream interruption is operationally distinct after response headers commit.
	if errors.Is(err, errSDKPackageStreamInterrupted) {
		code = "sdk_package_stream_interrupted"
	} else {
		var httpErr workspaceConfigHTTPError
		// Reviewed typed codes retain actionable cache-regeneration classification.
		if errors.As(err, &httpErr) && strings.TrimSpace(httpErr.code) != "" {
			code = httpErr.code
		}
	}
	span.SetAttributes(attribute.String("outcome", "failed"), attribute.String("error.code", code))
	span.SetStatus(codes.Error, code)
}

// serveSDKPackage checks Engine-owned delivery authority before any Registry cache or recovery request.
func serveSDKPackage(ctx context.Context, w http.ResponseWriter, apiKey string, actor accesscontrol.Actor, appID uuid.UUID, s store.Store, proxy Forwarder, packages SDKPackageClient) error {
	request, err := admittedSDKPackageRequest(ctx, s, actor.AccountID, appID)
	// Delivery eligibility is authoritative before any dependency lookup.
	if err != nil {
		return err
	}
	return serveAdmittedSDKPackage(ctx, w, apiKey, actor, appID, request, proxy, packages)
}

// admittedSDKPackageRequest keeps store identity and delivery-mode failures separate from Registry recovery.
func admittedSDKPackageRequest(ctx context.Context, s store.Store, accountID, appID uuid.UUID) (*models.SDKGenerationRequest, error) {
	buildStore, ok := s.(store.SDKPackageBuildStore)
	// A missing store capability cannot prove package eligibility.
	if !ok {
		return nil, sdkPackageDependencyError("SDK package recovery is unavailable")
	}
	request, err := buildStore.GetSDKPackageBuildRequest(ctx, accountID, appID)
	// Direct API delivery is a permanent user-facing boundary, not a dependency outage.
	if errors.Is(err, store.ErrSDKPackageNotGenerated) {
		return nil, workspaceConfigHTTPError{status: http.StatusConflict, code: "sdk_package_not_generated", category: "conflict", message: "This direct API app has no generated SDK package.", remediation: "Invoke the API directly, or initialize a new app with --sdk to generate a package."}
	}
	// Keep missing and unauthorized identities opaque.
	if errors.Is(err, store.ErrAppNotFound) {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "SDK app version not found"}
	}
	// Storage failure must not trigger cache recovery.
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load SDK package definition"}
	}
	return request, nil
}

// serveAdmittedSDKPackage performs one bounded cache recovery only after Engine proves generated delivery.
func serveAdmittedSDKPackage(ctx context.Context, w http.ResponseWriter, apiKey string, actor accesscontrol.Actor, appID uuid.UUID, request *models.SDKGenerationRequest, proxy Forwarder, packages SDKPackageClient) error {
	// Registry availability matters only after generated-package eligibility is established.
	if packages == nil {
		return sdkPackageDependencyError("SDK package recovery is unavailable")
	}
	response, err := packages.DownloadSDKPackage(ctx, appID)
	// A transport failure is not an authoritative cache miss.
	if err != nil {
		return sdkPackageDependencyError("The Registry could not load this SDK package")
	}
	// Only a definite miss may enter exact-version recovery.
	if response.StatusCode != http.StatusNotFound {
		return streamSDKPackageResponse(w, response)
	}
	response.Body.Close()
	// Recovery retains the pinned generator and must fail closed on rejection.
	if err := regenerateSDKPackage(ctx, proxy, apiKey, actor, request); err != nil {
		return err
	}
	retry, err := packages.DownloadSDKPackage(ctx, appID)
	// Do not reinterpret a failed second read as another regeneration opportunity.
	if err != nil {
		return sdkPackageDependencyError("The regenerated SDK package could not be loaded")
	}
	// One recovery attempt is the bound; missing output cannot loop indefinitely.
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

// safeSDKPackageGenerationError promotes only the reviewed pinned-generator
// conflict while leaving every unrecognized Registry response opaque.
func safeSDKPackageGenerationError(err error) error {
	var proxyErr sdkProxyError
	// Only the conflict returned by SDK generation can carry the reviewed code.
	if !errors.As(err, &proxyErr) || proxyErr.status != http.StatusConflict {
		return err
	}
	code := registrySDKGenerationErrorCode(proxyErr.body)
	// Exact allowlisting prevents arbitrary Registry prose or codes from reaching callers.
	if code == "sdk_generator_version_unavailable" {
		return workspaceConfigHTTPError{
			status: http.StatusConflict, code: code,
			message:  "This SDK version uses a generator that is no longer available.",
			category: "dependency", retryable: false,
		}
	}
	return err
}

// registrySDKGenerationErrorCode reads only the current nested Registry
// envelope without accepting legacy shapes or any accompanying text.
func registrySDKGenerationErrorCode(payload []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	// Malformed or absent error payloads cannot provide a reviewed code.
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Error) == 0 {
		return ""
	}
	var nested struct {
		Code string `json:"code"`
	}
	// The shared Registry control-plane envelope is the authoritative shape.
	if json.Unmarshal(envelope.Error, &nested) == nil && nested.Code != "" {
		return nested.Code
	}
	return ""
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
