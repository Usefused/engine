package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type executionAppSource struct {
	scope    *store.AppRuntime
	identity auth.RuntimeIdentity
	appID    uuid.UUID
	result   *store.ExecutionResult
}

// loadExecutionAppSource enforces the same app token and read handle for rerun and replay.
func (s *EngineGRPCServer) loadExecutionAppSource(ctx context.Context, request *http.Request) (executionAppSource, *restExecutionError) {
	appID, requestErr := parseRESTAppID(chi.URLParam(request, "app_id"))
	if requestErr != nil {
		return executionAppSource{}, requestErr
	}
	scope, identity, requestErr := s.authenticateRESTApp(request.WithContext(ctx), appID)
	if requestErr != nil {
		return executionAppSource{}, requestErr
	}
	// Rerun and replay belong to authored Execution Apps, never ordinary SDK raw results.
	if scope.Kind != store.AppKindExecution {
		return executionAppSource{}, newRESTExecutionError(http.StatusForbidden, "app_scope_unavailable", "Execution App scope is unavailable")
	}
	// A retained source is readable after promotion, but replay and rerun create new traffic.
	if requestErr := s.admitExecutionAppTraffic(ctx, appID); requestErr != nil {
		return executionAppSource{}, requestErr
	}
	sourceID, err := uuid.Parse(chi.URLParam(request, "execution_id"))
	// A malformed source cannot select another app's retained input or replay evidence.
	if err != nil || sourceID == uuid.Nil {
		return executionAppSource{}, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execution_id must be a UUID")
	}
	result, requestErr := s.authorizedExecutionResult(ctx, request, identity.AccountID, appID, sourceID)
	if requestErr != nil {
		return executionAppSource{}, requestErr
	}
	return executionAppSource{scope: scope, identity: identity, appID: appID, result: result}, nil
}

// handleCapabilityRerun starts a new live execution from retained input only after explicit caller authorization.
func (s *EngineGRPCServer) handleCapabilityRerun(writer http.ResponseWriter, request *http.Request) {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_app.rerun")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "caller"), attribute.String("execution.mode", "rerun"))
	source, requestErr := s.loadExecutionAppSource(ctx, request)
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	// A still-running source has no stable retained input/outcome for explicit rerun.
	if source.result.Status == "queued" || source.result.Status == "running" {
		writeCapabilityError(writer, span, newRESTExecutionError(http.StatusConflict, "execution_pending", "execution has not completed"))
		return
	}
	keyHash, requestErr := rerunIdempotencyKeyHash(request)
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	bundle, manifest, found, requestErr := s.findExecutionAppBundle(ctx, source.appID)
	// Rerun requires the same exact-version authored bundle that created the source.
	if !found && requestErr == nil {
		requestErr = newRESTExecutionError(http.StatusNotFound, "bundle_not_found", "execution app bundle is unavailable")
	}
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	result, requestErr := s.executeCapabilityRun(ctx, capabilityRunSpec{
		identity: source.identity, version: source.scope.Version, bundle: bundle,
		manifest: manifest, input: source.result.Input, mode: "rerun",
		sourceExecutionID: &source.result.ID, idempotencyKeyHash: keyHash,
	})
	writeCapabilityRun(writer, span, result, requestErr)
}

// rerunIdempotencyKeyHash binds duplicate submissions without persisting the caller's raw key.
func rerunIdempotencyKeyHash(request *http.Request) (string, *restExecutionError) {
	values := request.Header.Values("Idempotency-Key")
	// Exactly one bounded visible key is required because a rerun may repeat provider mutations.
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 256 || strings.TrimSpace(values[0]) != values[0] {
		return "", newRESTExecutionError(http.StatusBadRequest, "idempotency_key_required", "a valid Idempotency-Key is required")
	}
	for _, character := range values[0] {
		// Control characters cannot cross a header or become a stable retry identity.
		if character < 0x21 || character > 0x7e {
			return "", newRESTExecutionError(http.StatusBadRequest, "idempotency_key_required", "a valid Idempotency-Key is required")
		}
	}
	digest := sha256.Sum256([]byte(values[0]))
	return hex.EncodeToString(digest[:]), nil
}
