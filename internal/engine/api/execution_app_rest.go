package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type capabilityExecutionEnvelope struct {
	ExecutionID       uuid.UUID               `json:"executionId"`
	AppID             uuid.UUID               `json:"appId"`
	Version           string                  `json:"version"`
	Status            string                  `json:"status"`
	Mode              string                  `json:"mode"`
	SourceExecutionID *uuid.UUID              `json:"sourceExecutionId,omitempty"`
	ReadHandle        string                  `json:"readHandle,omitempty"`
	Output            json.RawMessage         `json:"output,omitempty"`
	Data              json.RawMessage         `json:"data"`
	Error             *restExecutionErrorBody `json:"error,omitempty"`
	CreatedAt         time.Time               `json:"createdAt"`
	CompletedAt       *time.Time              `json:"completedAt,omitempty"`
}

// MountExecutionAppRoutes adds rerun and replay to the existing SDK execution endpoint.
func MountExecutionAppRoutes(router chi.Router, server *EngineGRPCServer) {
	// A missing server cannot accidentally publish a route without authentication.
	if router == nil || server == nil {
		return
	}
	router.Post("/v1/apps/{app_id}/executions/{execution_id}/rerun", server.handleCapabilityRerun)
	router.Post("/v1/apps/{app_id}/executions/{execution_id}/replay", server.handleCapabilityReplay)
}

// tryExecutionAppRun uses the existing SDK POST when the exact app version has an authored execute bundle.
func (s *EngineGRPCServer) tryExecutionAppRun(writer http.ResponseWriter, request *http.Request, scope *store.AppRuntime, identity auth.RuntimeIdentity, decoded restExecutionRequest) bool {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_app.execute")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "caller"), attribute.String("execution.mode", "live"))
	bundle, manifest, found, requestErr := s.findExecutionAppBundle(ctx, identity.AppID)
	// Without a bundle the existing raw operation named execute remains available.
	if !found && requestErr == nil {
		return false
	}
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return true
	}
	requestErr = validateExecutionAppRESTControls(decoded)
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return true
	}
	spec := capabilityRunSpec{
		identity: identity, version: scope.Version, bundle: bundle,
		manifest: manifest, input: decoded.Input, mode: "live",
	}
	wait, requested, requestErr := capabilityWaitBudget(request)
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return true
	}
	var result capabilityExecutionEnvelope
	// A caller asking for a wait budget receives a durable pending handle when that budget ends.
	if requested {
		result, requestErr = s.executeCapabilityRunWithWait(ctx, spec, wait)
	} else {
		result, requestErr = s.executeCapabilityRun(ctx, spec)
	}
	writeCapabilityRun(writer, span, result, requestErr)
	return true
}

// admitExecutionAppTraffic rejects old exact versions after their family promotes a ready replacement.
func (s *EngineGRPCServer) admitExecutionAppTraffic(ctx context.Context, appID uuid.UUID) *restExecutionError {
	targets, ok := s.store.(store.ExecutionAppTargetStore)
	// A missing authoritative target store cannot establish the active version.
	if !ok {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution app target is unavailable")
	}
	active, err := targets.IsExecutionAppTrafficTarget(ctx, appID)
	// Persistence errors cannot permit execution through a stale app ID.
	if err != nil {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution app target is unavailable")
	}
	// A promoted sibling may keep historical records while this version stops receiving traffic.
	if !active {
		return newRESTExecutionError(http.StatusConflict, "app_version_not_current", "execution app version is not current")
	}
	return nil
}

// validateExecutionAppRESTControls keeps provider routing options out of the one authored input contract.
func validateExecutionAppRESTControls(request restExecutionRequest) *restExecutionError {
	// The authored script owns its selected workspace calls and selectors.
	if len(request.Targets) > 0 || request.Selector != nil || len(request.Selectors) > 0 || request.Pagination != nil || len(request.TargetPagination) > 0 {
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execute accepts only operation and input")
	}
	return nil
}

// findExecutionAppBundle pins one optional authored bundle to the exact authenticated app version.
func (s *EngineGRPCServer) findExecutionAppBundle(ctx context.Context, appID uuid.UUID) (*store.ExecutionAppBundle, *executionAppManifest, bool, *restExecutionError) {
	bundles, ok := s.store.(store.ExecutionAppBundleStore)
	// Without bundle storage, ordinary SDK versions can still run raw operations.
	if !ok {
		return s.missingExecutionAppBundle(ctx, appID)
	}
	bundle, err := bundles.GetExecutionAppBundle(ctx, appID)
	// A planned but unattached bundle may never fall through to an unrelated raw execute operation.
	if errors.Is(err, store.ErrExecutionAppBundleNotFound) {
		return s.missingExecutionAppBundle(ctx, appID)
	}
	if err != nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "execution app bundle is unavailable")
	}
	app, err := s.store.GetApp(ctx, appID)
	// A bundle attached to the wrong source identity cannot gain execution authority.
	if err != nil || app == nil || app.SourceHash != bundle.SourceHash || app.BundleDigest != store.ExecutionAppBundleDigest([]byte(bundle.BundleJS)) {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_invalid", "execution app bundle is invalid")
	}
	manifest, err := parseExecutionAppManifest(bundle.Manifest)
	// Corrupt descriptors fail closed before any execution record or provider call.
	if err != nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_invalid", "execution app bundle is invalid")
	}
	return bundle, manifest, true, nil
}

// missingExecutionAppBundle permits raw execute only when the immutable app version never planned hosted code.
func (s *EngineGRPCServer) missingExecutionAppBundle(ctx context.Context, appID uuid.UUID) (*store.ExecutionAppBundle, *executionAppManifest, bool, *restExecutionError) {
	app, err := s.store.GetApp(ctx, appID)
	// A failed identity read cannot prove that raw execute is the intended version contract.
	if err != nil || app == nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "execution app bundle is unavailable")
	}
	// Hosted code approved at apply must be attached before this app can run execute.
	if app.BundleDigest != "" {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "execution app bundle is unavailable")
	}
	return nil, nil, false, nil
}

// newExecutionReadHandle creates an unguessable handle while storing only its digest.
func newExecutionReadHandle() (string, string, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", err
	}
	handle := hex.EncodeToString(random[:])
	digest := sha256.Sum256([]byte(handle))
	return handle, hex.EncodeToString(digest[:]), nil
}

// capabilityCompletion distinguishes an interrupted provider call from an ordinary authored failure.
func capabilityCompletion(runErr, ctxErr error, providerStarted bool) (string, string) {
	// A completed, validated output is the only success state.
	if runErr == nil {
		return "succeeded", ""
	}
	// Cancellation after provider dispatch leaves its outcome uncertain.
	if ctxErr != nil && providerStarted {
		return "indeterminate", "execution_interrupted"
	}
	return "failed", "execution_failed"
}

// capabilityPublicError maps private authored/provider failures to stable public codes.
func capabilityPublicError(code string) string {
	// Successful records carry no public error message.
	if code == "" {
		return ""
	}
	return "execution app did not complete successfully"
}

// projectCapabilityExecution adds the one-time read handle without exposing input or token identity.
func projectCapabilityExecution(record *store.ExecutionResult, readHandle string) capabilityExecutionEnvelope {
	projected := capabilityExecutionEnvelope{
		ExecutionID: record.ID, AppID: record.AppID, Version: record.AppVersion,
		Status: record.Status, Mode: record.Mode,
		SourceExecutionID: record.SourceExecutionID, ReadHandle: readHandle,
		Output: record.Output, Data: record.Data,
		CreatedAt: record.CreatedAt, CompletedAt: record.CompletedAt,
	}
	// Errors have a bounded public projection distinct from private provider diagnostics.
	if record.ErrorCode != "" {
		projected.Error = &restExecutionErrorBody{Code: record.ErrorCode, Message: record.ErrorMessage}
	}
	return projected
}

// writeCapabilityRun publishes the common execution envelope and bounded logical OTEL outcome.
func writeCapabilityRun(writer http.ResponseWriter, span trace.Span, result capabilityExecutionEnvelope, requestErr *restExecutionError) {
	// Rejections have no accepted execution ID and use the standard REST error projection.
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	span.SetAttributes(attribute.String("app.id", result.AppID.String()),
		attribute.String("execution.id", result.ExecutionID.String()), attribute.String("execution.status", result.Status))
	// A failed authored run still returns its durable ID while OTEL marks the logical outcome.
	if result.Error != nil {
		span.SetStatus(codes.Error, result.Error.Code)
	}
	status := http.StatusOK
	// A queued or running execution is retrievable by ID while its worker continues.
	if result.Status == "queued" || result.Status == "running" {
		status = http.StatusAccepted
	}
	writeRESTExecutionJSON(writer, status, result)
}

// writeCapabilityError records a bounded OTEL failure before using the shared REST error envelope.
func writeCapabilityError(writer http.ResponseWriter, span trace.Span, err *restExecutionError) {
	// Handler errors are always classified without copying request or provider content into telemetry.
	if err == nil {
		return
	}
	span.SetAttributes(attribute.String("execution.status", "rejected"), attribute.String("error.code", err.code))
	span.SetStatus(codes.Error, err.code)
	writeRESTExecutionError(writer, err)
}
