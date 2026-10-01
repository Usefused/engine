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
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/sandbox"
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
	AppID             uuid.UUID               `json:"-"`
	AppFamilyID       uuid.UUID               `json:"appFamilyId"`
	Version           string                  `json:"version"`
	Status            string                  `json:"status"`
	Mode              string                  `json:"mode"`
	SourceExecutionID *uuid.UUID              `json:"sourceExecutionId,omitempty"`
	ReadHandle        string                  `json:"readHandle,omitempty"`
	Output            json.RawMessage         `json:"output,omitempty"`
	Error             *restExecutionErrorBody `json:"error,omitempty"`
	CreatedAt         time.Time               `json:"createdAt"`
	CompletedAt       *time.Time              `json:"completedAt,omitempty"`
}

// MountUnifiedAppRoutes adds rerun and replay to the existing SDK execution endpoint.
func MountUnifiedAppRoutes(router chi.Router, server *EngineGRPCServer) {
	// A missing server cannot accidentally publish a route without authentication.
	if router == nil || server == nil {
		return
	}
	router.Post("/v1/apps/{app_id}/executions/{execution_id}/rerun", server.handleCapabilityRerun)
	router.Post("/v1/apps/{app_id}/executions/{execution_id}/replay", server.handleCapabilityReplay)
}

// tryUnifiedAppRun uses the existing SDK POST when the exact app version has an authored execute bundle.
func (s *EngineGRPCServer) tryUnifiedAppRun(writer http.ResponseWriter, request *http.Request, scope *store.AppRuntime, identity auth.RuntimeIdentity, decoded restExecutionRequest) bool {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.unified_app.execute")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "caller"), attribute.String("execution.mode", "live"))
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, identity.AppID)
	// Without a bundle the existing raw operation named execute remains available.
	if !found && requestErr == nil {
		return false
	}
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return true
	}
	requestErr = validateUnifiedAppRESTControls(decoded)
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

// admitUnifiedAppTraffic rejects old exact versions after their family promotes a ready replacement.
func (s *EngineGRPCServer) admitUnifiedAppTraffic(ctx context.Context, appID uuid.UUID) *restExecutionError {
	targets, ok := s.store.(store.UnifiedAppTargetStore)
	// A missing authoritative target store cannot establish the active version.
	if !ok {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "unified app target is unavailable")
	}
	active, err := targets.IsUnifiedAppTrafficTarget(ctx, appID)
	// Persistence errors cannot permit execution through a stale app ID.
	if err != nil {
		return newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "unified app target is unavailable")
	}
	// A promoted sibling may keep historical records while this version stops receiving traffic.
	if !active {
		return newRESTExecutionError(http.StatusConflict, "app_version_not_current", "unified app version is not current")
	}
	return nil
}

// validateUnifiedAppRESTControls keeps provider routing options out of the one authored input contract.
func validateUnifiedAppRESTControls(request restExecutionRequest) *restExecutionError {
	// The authored script owns its selected workspace calls and selectors.
	if request.Selector != nil || request.Pagination != nil {
		return newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execute accepts only operation and input")
	}
	return nil
}

// findUnifiedAppBundle pins one optional authored bundle to the exact authenticated app version.
func (s *EngineGRPCServer) findUnifiedAppBundle(ctx context.Context, appID uuid.UUID) (*store.UnifiedAppBundle, *unifiedAppManifest, bool, *restExecutionError) {
	bundles, ok := s.store.(store.UnifiedAppBundleStore)
	// Without bundle storage, ordinary SDK versions can still run raw operations.
	if !ok {
		return s.missingUnifiedAppBundle(ctx, appID)
	}
	bundle, err := bundles.GetUnifiedAppBundle(ctx, appID)
	// A planned but unattached bundle may never fall through to an unrelated raw execute operation.
	if errors.Is(err, store.ErrUnifiedAppBundleNotFound) {
		return s.missingUnifiedAppBundle(ctx, appID)
	}
	if err != nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "unified app bundle is unavailable")
	}
	app, err := s.store.GetApp(ctx, appID)
	// A bundle attached to the wrong source identity cannot gain execution authority.
	if err != nil || app == nil || app.SourceHash != bundle.SourceHash || app.BundleDigest != store.UnifiedAppBundleDigest([]byte(bundle.BundleJS)) {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_invalid", "unified app bundle is invalid")
	}
	manifest, err := parseUnifiedAppManifest(bundle.Manifest)
	// Corrupt descriptors fail closed before any execution record or provider call.
	if err != nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_invalid", "unified app bundle is invalid")
	}
	return bundle, manifest, true, nil
}

// missingUnifiedAppBundle permits raw execute only when the immutable app version never planned hosted code.
func (s *EngineGRPCServer) missingUnifiedAppBundle(ctx context.Context, appID uuid.UUID) (*store.UnifiedAppBundle, *unifiedAppManifest, bool, *restExecutionError) {
	app, err := s.store.GetApp(ctx, appID)
	// A failed identity read cannot prove that raw execute is the intended version contract.
	if err != nil || app == nil {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "unified app bundle is unavailable")
	}
	// Hosted code approved at apply must be attached before this app can run execute.
	if app.BundleDigest != "" {
		return nil, nil, true, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_unavailable", "unified app bundle is unavailable")
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
	// Admission failures happen before authored code and remain safe, actionable public categories.
	if errors.Is(runErr, sandbox.ErrCapabilityAdmissionFull) || errors.Is(runErr, sandbox.ErrCapabilityWorkerOverloaded) {
		return "failed", "execution_capacity_exceeded"
	}
	if errors.Is(runErr, sandbox.ErrCapabilityAdmissionTimeout) {
		return "failed", "execution_queue_timeout"
	}
	// Error categories are bounded metadata; exception messages remain private.
	var diagnostic *executionappvm.DiagnosticError
	if errors.As(runErr, &diagnostic) {
		switch diagnostic.Phase {
		case "timeout":
			// Timed-out provider work may have committed a side effect and must not imply retry safety.
			if providerStarted {
				return "indeterminate", "execution_interrupted"
			}
			return "failed", "execution_timeout"
		case "input_validation":
			return "failed", "input_validation_failed"
		case "output_validation":
			return "failed", "output_validation_failed"
		}
	}
	return "failed", "execution_failed"
}

// capabilityPublicError maps private authored/provider failures to stable public codes.
func capabilityPublicError(code string) string {
	// Successful records carry no public error message.
	if code == "" {
		return ""
	}
	// Infrastructure queue failures disclose no authored input, provider output, or exception details.
	switch code {
	case "execution_capacity_exceeded":
		return "Unified App execution capacity is full; try again later."
	case "execution_queue_timeout":
		return "Unified App execution did not start before the queue wait expired."
	}
	return "unified app did not complete successfully"
}

// projectCapabilityExecution returns the authored output and one-time read handle while keeping stored search data private.
func projectCapabilityExecution(record *store.ExecutionResult, readHandle string) capabilityExecutionEnvelope {
	projected := capabilityExecutionEnvelope{
		ExecutionID: record.ID, AppID: record.AppID, AppFamilyID: record.AppFamilyID, Version: record.AppVersion,
		Status: record.Status, Mode: record.Mode,
		SourceExecutionID: record.SourceExecutionID, ReadHandle: readHandle,
		Output:    record.Output,
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
