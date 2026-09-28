package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// handleCapabilityReplay runs the retained bundle and input against recorded calls without provider access.
func (s *EngineGRPCServer) handleCapabilityReplay(writer http.ResponseWriter, request *http.Request) {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_app.replay")
	defer span.End()
	span.SetAttributes(attribute.String("execution.trigger", "caller"), attribute.String("execution.mode", "replay"))
	source, requestErr := s.loadExecutionAppSource(ctx, request)
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	// Pending and replay results have no admissible live transcript of their own.
	if source.result.Status == "queued" || source.result.Status == "running" || source.result.Status == "indeterminate" || source.result.Mode == "replay" {
		writeCapabilityError(writer, span, newRESTExecutionError(http.StatusConflict, "replay_unavailable", "execution cannot be replayed"))
		return
	}
	bundle, manifest, found, requestErr := s.findExecutionAppBundle(ctx, source.appID)
	// Replay cannot fall back to raw SDK operations when its authored bundle is absent.
	if !found && requestErr == nil {
		requestErr = newRESTExecutionError(http.StatusNotFound, "bundle_not_found", "execution app bundle is unavailable")
	}
	if requestErr != nil {
		writeCapabilityError(writer, span, requestErr)
		return
	}
	// The retained source must belong to the same immutable version selected by this token.
	if source.result.AppVersion != source.scope.Version {
		writeCapabilityError(writer, span, newRESTExecutionError(http.StatusConflict, "replay_unavailable", "execution cannot be replayed"))
		return
	}
	result, requestErr := s.executeCapabilityReplay(ctx, source.identity, source.scope.Version, bundle, manifest, source.result)
	writeCapabilityRun(writer, span, result, requestErr)
}

// executeCapabilityReplay records a new result while the replay host serves all effects from memory.
func (s *EngineGRPCServer) executeCapabilityReplay(ctx context.Context, identity auth.RuntimeIdentity, version string, bundle *store.ExecutionAppBundle, manifest *executionAppManifest, source *store.ExecutionResult) (capabilityExecutionEnvelope, *restExecutionError) {
	evidence, ok := s.store.(store.ExecutionReplayEvidenceStore)
	// An installation without encrypted evidence cannot silently execute the provider again.
	if !ok {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusConflict, "replay_unavailable", "execution cannot be replayed")
	}
	history, err := evidence.GetReplayEvidence(ctx, identity.AccountID, identity.AppID, source.ID, s.masterKey)
	if err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusConflict, "replay_unavailable", "execution cannot be replayed")
	}
	host, err := NewReplayCapabilityHost(history)
	if err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusConflict, "replay_unavailable", "execution cannot be replayed")
	}
	spec := capabilityRunSpec{identity: identity, version: version, bundle: bundle, manifest: manifest, input: source.Input, mode: "replay", sourceExecutionID: &source.ID}
	admitted, requestErr := s.admitCapabilityRun(ctx, spec)
	if requestErr != nil {
		return capabilityExecutionEnvelope{}, requestErr
	}
	// The replay worker receives no runtime cache or live workspace host, so provider calls are impossible.
	if err := admitted.results.StartExecutionResult(ctx, identity.AccountID, identity.AppID, admitted.id); err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	output, runErr := s.runExecutionAppWorker(ctx, identity, []byte(bundle.BundleJS), source.Input, host, host.Determinism())
	status, code := replayCompletion(source, output, host, runErr)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// The temporary replay document commits only with this new result's terminal state.
	if err := admitted.results.CompleteExecutionResult(finishCtx, identity.AccountID, identity.AppID, admitted.id, status, output, host.Data(), code, capabilityPublicError(code)); err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	return loadCapabilityRunEnvelope(finishCtx, admitted.results, identity, admitted.id, admitted.readHandle)
}

// replayCompletion requires matching calls, output, data, and status before reporting a reproducible result.
func replayCompletion(source *store.ExecutionResult, output json.RawMessage, host *replayCapabilityHost, runErr error) (string, string) {
	// Divergent or incomplete host use is an explicit replay failure, even if authored code handled the error.
	if host.Consumed() != nil || !sameCapabilityJSON(output, source.Output) || !sameCapabilityJSON(host.Data(), source.Data) || (runErr == nil) != (source.Status == "succeeded") {
		return "failed", "replay_diverged"
	}
	// A matching failed source remains a failed replay rather than being reclassified as success.
	if runErr != nil {
		return "failed", source.ErrorCode
	}
	return "succeeded", ""
}

// sameCapabilityJSON compares JSON meaning without depending on JSONB key order.
func sameCapabilityJSON(left, right json.RawMessage) bool {
	leftValue, leftErr := canonicalReplayValue(left, store.MaxExecutionDataBytes)
	rightValue, rightErr := canonicalReplayValue(right, store.MaxExecutionDataBytes)
	// Invalid or oversized retained values cannot be accepted as a matching replay.
	return leftErr == nil && rightErr == nil && bytes.Equal(leftValue, rightValue)
}
