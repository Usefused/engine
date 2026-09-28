package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type capabilityRunSpec struct {
	identity           auth.RuntimeIdentity
	version            string
	bundle             *store.ExecutionAppBundle
	manifest           *executionAppManifest
	input              json.RawMessage
	mode               string
	sourceExecutionID  *uuid.UUID
	idempotencyKeyHash string
}

type admittedCapabilityRun struct {
	results    store.ExecutionResultStore
	bindings   map[string]sandbox.ExactOperationBinding
	id         uuid.UUID
	readHandle string
	created    bool
}

// executeCapabilityRun records and executes one live or rerun invocation against an exact app bundle.
func (s *EngineGRPCServer) executeCapabilityRun(ctx context.Context, spec capabilityRunSpec) (capabilityExecutionEnvelope, *restExecutionError) {
	admitted, requestErr := s.admitCapabilityRun(ctx, spec)
	if requestErr != nil {
		return capabilityExecutionEnvelope{}, requestErr
	}
	// A repeated rerun key returns the existing execution without another provider effect.
	if !admitted.created {
		return loadCapabilityRunEnvelope(ctx, admitted.results, spec.identity, admitted.id, "")
	}
	return s.runAdmittedCapability(ctx, spec, admitted)
}

// runAdmittedCapability owns provider work only after the result reservation is durable.
func (s *EngineGRPCServer) runAdmittedCapability(ctx context.Context, spec capabilityRunSpec, admitted admittedCapabilityRun) (capabilityExecutionEnvelope, *restExecutionError) {
	controls, err := sandbox.NewCapabilityDeterminism()
	// No provider work may start if replay controls could not be generated for this invocation.
	if err != nil {
		return failCapabilityRuntimeStart(ctx, admitted, spec)
	}
	// DB-only scripts have no physical bindings and need no provider cache or workspace connection.
	if len(admitted.bindings) > 0 {
		// A fresh reservation alone can acquire provider-capable cache scope.
		if err := s.restRuntime.ConnectAppRuntime(ctx, spec.identity.AppID); err != nil {
			return failCapabilityRuntimeStart(ctx, admitted, spec)
		}
		// The runtime cache remains acquired for every provider call in the script.
		defer s.restRuntime.DisconnectAppRuntime(spec.identity.AppID)
	}
	if err := admitted.results.StartExecutionResult(ctx, spec.identity.AccountID, spec.identity.AppID, admitted.id); err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	host := &executionCapabilityHost{
		runtime: s.runtime, identity: spec.identity,
		executionID: admitted.id, bindings: admitted.bindings,
	}
	// Buffering the JSON document keeps the final state and output in one terminal SQL update.
	buffer := NewBufferedCapabilityHost(host)
	// Recording every workspace call gives replay a sealed transcript without exposing provider credentials.
	recorder := NewRecordingCapabilityHost(buffer)
	setErr := recorder.SetDeterminism(controls)
	var output json.RawMessage
	var runErr error
	// A malformed control record cannot run code whose effects would lack deterministic replay evidence.
	if setErr != nil {
		runErr = setErr
	} else {
		output, runErr = s.runExecutionAppWorker(ctx, spec.identity, []byte(spec.bundle.BundleJS), spec.input, recorder, controls)
	}
	history, historyErr := recorder.History()
	// An incomplete recording cannot be presented as a replayable success.
	if historyErr != nil && runErr == nil {
		runErr = historyErr
	}
	status, errorCode := capabilityCompletion(runErr, ctx.Err(), host.providerCallsStarted())
	// Terminal state survives client cancellation and remains attached to its original trace.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if err := admitted.results.CompleteExecutionResult(finishCtx, spec.identity.AccountID, spec.identity.AppID, admitted.id, status, output, buffer.Data(), errorCode, capabilityPublicError(errorCode)); err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	s.saveExecutionAppReplayEvidence(finishCtx, spec, admitted.id, history, historyErr)
	return loadCapabilityRunEnvelope(finishCtx, admitted.results, spec.identity, admitted.id, admitted.readHandle)
}

// saveExecutionAppReplayEvidence seals complete history only after the matching result commits.
func (s *EngineGRPCServer) saveExecutionAppReplayEvidence(ctx context.Context, spec capabilityRunSpec, executionID uuid.UUID, history json.RawMessage, historyErr error) {
	// Incomplete history cannot authenticate a future side-effect-free replay.
	if historyErr != nil {
		return
	}
	evidence, ok := s.store.(store.ExecutionReplayEvidenceStore)
	if !ok {
		return
	}
	// The live result remains fetchable if only encrypted replay evidence could not be sealed.
	if err := evidence.SaveReplayEvidence(ctx, spec.identity.AccountID, spec.identity.AppID, executionID, history, s.masterKey); err != nil {
		span := trace.SpanFromContext(ctx)
		span.SetAttributes(attribute.Bool("execution.replay_available", false))
		span.SetStatus(codes.Error, "replay_evidence_unavailable")
	}
}

// admitCapabilityRun validates storage and selected operations before a provider-capable worker starts.
func (s *EngineGRPCServer) admitCapabilityRun(ctx context.Context, spec capabilityRunSpec) (admittedCapabilityRun, *restExecutionError) {
	results, ok := s.store.(store.ExecutionResultStore)
	// Missing durable storage or an authored contract prevents unrecorded effects.
	if !ok || spec.bundle == nil || spec.manifest == nil {
		return admittedCapabilityRun{}, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution app runtime is unavailable")
	}
	bindings, err := executionAppBindings(spec.manifest)
	if err != nil {
		return admittedCapabilityRun{}, newRESTExecutionError(http.StatusServiceUnavailable, "bundle_invalid", "execution app bundle is invalid")
	}
	// Provider-capable scripts require both the physical dispatcher and its request-scoped cache.
	if len(bindings) > 0 && (s.runtime == nil || s.restRuntime == nil) {
		return admittedCapabilityRun{}, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution app runtime is unavailable")
	}
	id, handle, created, err := reserveCapabilityRun(ctx, results, spec)
	// A rejected reservation cannot acquire a cache reference or start provider work.
	if err != nil {
		// The same caller key cannot name two different retained inputs or app versions.
		if errors.Is(err, store.ErrExecutionResultIdempotencyConflict) {
			return admittedCapabilityRun{}, newRESTExecutionError(http.StatusConflict, "idempotency_conflict", "idempotency key belongs to a different rerun")
		}
		return admittedCapabilityRun{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	return admittedCapabilityRun{results: results, bindings: bindings, id: id, readHandle: handle, created: created}, nil
}

// failCapabilityRuntimeStart preserves the accepted ID when no worker cache can be acquired.
func failCapabilityRuntimeStart(ctx context.Context, admitted admittedCapabilityRun, spec capabilityRunSpec) (capabilityExecutionEnvelope, *restExecutionError) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// No provider call began, so this queued execution has a known failure outcome.
	if err := admitted.results.CompleteExecutionResult(finishCtx, spec.identity.AccountID, spec.identity.AppID, admitted.id,
		"failed", nil, json.RawMessage("null"), "runtime_unavailable", "execution app runtime is unavailable"); err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	return loadCapabilityRunEnvelope(finishCtx, admitted.results, spec.identity, admitted.id, admitted.readHandle)
}

// reserveCapabilityRun creates the durable accepted record or finds an identical rerun reservation.
func reserveCapabilityRun(ctx context.Context, results store.ExecutionResultStore, spec capabilityRunSpec) (uuid.UUID, string, bool, error) {
	id := uuid.New()
	readHandle, readHash, err := newExecutionReadHandle()
	if err != nil {
		return uuid.Nil, "", false, err
	}
	record := store.ExecutionResult{
		ID: id, AccountID: spec.identity.AccountID, AppFamilyID: spec.identity.AppFamilyID,
		AppID: spec.identity.AppID, AppVersion: spec.version,
		AppTokenID: spec.identity.TokenID, ReadHandleHash: readHash,
		IdempotencyKeyHash: spec.idempotencyKeyHash, Status: "queued", Input: spec.input,
		Mode: spec.mode, SourceExecutionID: spec.sourceExecutionID,
	}
	// Live calls and side-effect-free replays each use a fresh one-time reservation.
	if spec.mode == "live" || spec.mode == "replay" {
		return id, readHandle, true, results.CreateExecutionResult(ctx, record)
	}
	// Only explicit reruns may reuse a previous reservation under one caller key.
	if spec.mode != "rerun" {
		return uuid.Nil, "", false, errors.New("unsupported execution app run mode")
	}
	reservedID, created, err := results.CreateOrGetRerunExecutionResult(ctx, record)
	// The first response owns the read handle; duplicates never reissue it.
	if !created {
		readHandle = ""
	}
	return reservedID, readHandle, created, err
}

// loadCapabilityRunEnvelope projects the durable result without recovering a one-time handle.
func loadCapabilityRunEnvelope(ctx context.Context, results store.ExecutionResultStore, identity auth.RuntimeIdentity, executionID uuid.UUID, readHandle string) (capabilityExecutionEnvelope, *restExecutionError) {
	record, err := results.GetExecutionResult(ctx, identity.AccountID, identity.AppID, executionID)
	// A missing row after reservation is a storage failure, not a successful execution.
	if err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(http.StatusServiceUnavailable, "result_unavailable", "execution result is unavailable")
	}
	return projectCapabilityExecution(record, readHandle), nil
}
