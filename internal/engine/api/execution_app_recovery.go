package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"net/http"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel/trace"
)

// trySnapshotRerun routes retained recovery-enabled runs through one shared attempt reservation.
func (s *EngineGRPCServer) trySnapshotRerun(w http.ResponseWriter, r *http.Request, span trace.Span, source unifiedAppSource) bool {
	repository, ok := s.store.(store.ExecutionRecoveryStore)
	// Historical fixtures and stores retain the existing rerun boundary until recovery is available.
	if !ok {
		return false
	}
	state, err := repository.GetExecutionRecovery(r.Context(), source.identity.AccountID, source.appID, source.result.ID)
	// Missing metadata denotes a historical execution with no durable snapshot contract.
	if err == nil && state == nil {
		return false
	}
	// A metadata failure must not fall back to a fresh, snapshot-free invocation.
	if err != nil {
		writeCapabilityError(w, span, newRESTExecutionError(503, "result_unavailable", "execution recovery is unavailable"))
		return true
	}
	key, requestErr := rerunIdempotencyKeyHash(r)
	// Manual retries retain the existing explicit idempotency-key requirement.
	if requestErr != nil {
		writeCapabilityError(w, span, requestErr)
		return true
	}
	result, requestErr := s.executeRecoveryAttempt(r.Context(), repository, *source.result, source.identity, key, false)
	writeCapabilityRun(w, span, result, requestErr)
	return true
}

// executeRecoveryAttempt reuses ordinary admission, confined execution, receipts and provider routing.
func (s *EngineGRPCServer) executeRecoveryAttempt(ctx context.Context, repository store.ExecutionRecoveryStore, source store.ExecutionResult, identity auth.RuntimeIdentity, key string, automatic bool) (capabilityExecutionEnvelope, *restExecutionError) {
	spec, requestErr := s.recoveryRunSpec(ctx, source, identity)
	// A retained snapshot never grants permission to run a retired or unavailable bundle.
	if requestErr != nil {
		return capabilityExecutionEnvelope{}, requestErr
	}
	prepared, requestErr := s.prepareCapabilityRun(ctx, spec)
	// Provider and credential routing must pass the same admission checks as ordinary calls.
	if requestErr != nil {
		return capabilityExecutionEnvelope{}, requestErr
	}
	record, created, err := repository.ReserveExecutionAttempt(ctx, store.ExecutionRerunRequest{AccountID: source.AccountID, AppID: source.AppID, SourceID: source.ID, TokenID: identity.TokenID, KeyHash: key, Automatic: automatic})
	// A changed owner or exhausted policy is a conflict, not a successful new run.
	if errors.Is(err, store.ErrExecutionRecoveryConflict) {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(409, "recovery_conflict", err.Error())
	}
	if err != nil {
		return capabilityExecutionEnvelope{}, newRESTExecutionError(503, "result_unavailable", "execution recovery is unavailable")
	} // Uncertain reservations never dispatch effects.
	// Duplicate requests return the same retained attempt without executing it again.
	if !created {
		return loadCapabilityRunEnvelope(ctx, prepared.results, identity, record.ID, "")
	}
	prepared.id, prepared.created = record.ID, true
	result, requestErr := s.runAdmittedCapability(ctx, spec, prepared)
	s.finishRecoverySchedule(ctx, record, result, requestErr)
	return result, requestErr
}

// recoveryRunSpec pins the original immutable input, version and runtime bucket on every attempt.
func (s *EngineGRPCServer) recoveryRunSpec(ctx context.Context, source store.ExecutionResult, identity auth.RuntimeIdentity) (capabilityRunSpec, *restExecutionError) {
	// Policy opt-in never overrides live Engine suspension or deployment state.
	if entitlement.EngineSuspended.Load() || entitlement.EngineHeartbeatLeaseExpired.Load() {
		return capabilityRunSpec{}, newRESTExecutionError(503, "runtime_unavailable", "Engine execution is unavailable")
	}
	if err := s.admitUnifiedAppTraffic(ctx, source.AppID); err != nil {
		return capabilityRunSpec{}, err
	} // Current traffic authority is required for the pinned version.
	bundle, manifest, found, requestErr := s.findUnifiedAppBundle(ctx, source.AppID)
	// Recovery cannot fall back to a promoted replacement or guessed bundle.
	if requestErr != nil {
		return capabilityRunSpec{}, requestErr
	}
	if !found {
		return capabilityRunSpec{}, newRESTExecutionError(404, "bundle_not_found", "unified app bundle is unavailable")
	}
	identity = recoveryOriginIdentity(source, identity)
	transport := "rest"
	// Original automation provenance survives retries in canonical activity.
	if source.SourceScheduleID != nil {
		transport = "schedule"
	}
	if source.SourceWebhookEventID != "" {
		transport = "webhook"
	} // Provider event runs keep their original trigger.
	return capabilityRunSpec{identity: identity, version: source.AppVersion, bundle: bundle, manifest: manifest, input: source.Input, mode: "rerun", transport: transport, bucketID: source.BucketID, sourceExecutionID: &source.ID, webhookEventID: source.SourceWebhookEventID}, nil
}

// finishRecoverySchedule updates only the scheduled occurrence still owned by this attempt.
func (s *EngineGRPCServer) finishRecoverySchedule(ctx context.Context, record *store.ExecutionResult, result capabilityExecutionEnvelope, requestErr *restExecutionError) {
	// Uncertain persistence leaves the running marker for the ordinary recovery sweep.
	if record.SourceScheduleID == nil || requestErr != nil {
		return
	}
	repository, ok := s.store.(store.UnifiedAppScheduleStore)
	// Optional partial fixtures do not own schedule persistence.
	if !ok {
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = repository.FinishScheduledExecution(finishCtx, *record.SourceScheduleID, record.ID, result.Status)
}

// runExecutionRecoveryBatch discovers and joins bounded retries inside the existing scheduler lifecycle.
func (s *EngineGRPCServer) runExecutionRecoveryBatch(ctx context.Context) {
	repository, ok := s.store.(store.ExecutionRecoveryStore)
	// Recovery shares Engine suspension gates and never starts effects during shutdown.
	if !ok || ctx.Err() != nil || entitlement.EngineSuspended.Load() || entitlement.EngineHeartbeatLeaseExpired.Load() {
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	records, err := repository.DueExecutionRecoveries(readCtx)
	cancel()
	// Database uncertainty cannot produce a partial or guessed recovery batch.
	if err != nil {
		return
	}
	var running sync.WaitGroup
	// The SQL batch is capped at 32 and shared runtime admission still owns execution capacity.
	for _, record := range records {
		running.Add(1)
		// Each occurrence has an independent bounded context and is joined before the next scheduler poll.
		go func(record store.ExecutionResult) {
			defer running.Done()
			s.runRecoveryRecord(ctx, repository, record)
		}(record)
	}
	running.Wait()
}

// handleCapabilityAttempts requires the same family token and read capability as ordinary result reads.
func (s *EngineGRPCServer) handleCapabilityAttempts(w http.ResponseWriter, r *http.Request) {
	endpoint, requestErr := s.resolveRESTAppReference(r)
	// An unresolved endpoint cannot establish tenant or family ownership.
	if requestErr != nil {
		writeRESTExecutionError(w, requestErr)
		return
	}
	scope, _, requestErr := s.authenticateRESTEndpoint(r, endpoint)
	// Token expiry and revocation are enforced on every history request.
	if requestErr != nil {
		writeRESTExecutionError(w, requestErr)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "execution_id"))
	// Malformed IDs share the ordinary non-disclosing result error.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(404, "execution_not_found", "execution not found"))
		return
	}
	record, requestErr := s.authorizedEndpointExecutionResult(r.Context(), r, scope, endpoint, id)
	// A family token alone does not grant access to another invocation's history.
	if requestErr != nil {
		writeRESTExecutionError(w, requestErr)
		return
	}
	repository, ok := s.store.(store.ExecutionRecoveryStore)
	// Partial fixtures cannot fabricate durable attempt history.
	if !ok {
		writeRESTExecutionError(w, newRESTExecutionError(503, "runtime_unavailable", "execution recovery is unavailable"))
		return
	}
	attempts, err := repository.ListExecutionAttempts(r.Context(), record.AccountID, record.AppID, record.ID)
	// Private snapshot data never appears in this metadata-only response.
	if err != nil {
		writeRESTExecutionError(w, newRESTExecutionError(503, "result_unavailable", "execution recovery is unavailable"))
		return
	}
	writeJSON(w, map[string]any{"attempts": attempts})
}

// runQueuedRecovery executes a manually reserved attempt once, through the shared executor.
func (s *EngineGRPCServer) runQueuedRecovery(ctx context.Context, repository store.ExecutionRecoveryStore, record store.ExecutionResult, identity auth.RuntimeIdentity) {
	spec, requestErr := s.recoveryRunSpec(ctx, record, identity)
	// A pending request cannot override current deployment or license authority.
	if requestErr != nil {
		return
	}
	admitted, requestErr := s.prepareCapabilityRun(ctx, spec)
	// Failed preparation leaves the durable request queued for a later safe attempt.
	if requestErr != nil {
		return
	}
	claimed, err := repository.ClaimQueuedExecutionAttempt(ctx, record.AccountID, record.AppID, record.ID)
	// Only the winner of the database claim may execute authored code.
	if err != nil || !claimed {
		return
	}
	admitted.id, admitted.created = record.ID, true
	result, requestErr := s.runAdmittedCapability(ctx, spec, admitted)
	s.finishRecoverySchedule(ctx, &record, result, requestErr)
}

// runRecoveryRecord reloads live grants and selects queued manual work or a policy-authorized automatic retry.
func (s *EngineGRPCServer) runRecoveryRecord(ctx context.Context, repository store.ExecutionRecoveryStore, record store.ExecutionResult) {
	attemptCtx, stop := context.WithTimeout(ctx, 45*time.Second)
	digest := sha256.Sum256([]byte("automatic:" + record.ID.String()))
	defer stop()
	projection, err := repository.AuthorizeExecutionRecovery(attemptCtx, record.AccountID, record.AppID, record.ID)
	// Autonomous work must reload live grants rather than assume the original token remains valid.
	if err != nil {
		return
	}
	identity := auth.RuntimeIdentity{AccountID: projection.AccountID, AppFamilyID: projection.AppFamilyID, AppID: projection.AppID, AppVersion: projection.Version, TokenID: projection.TokenID, Kind: projection.Kind, Status: projection.AppStatus, TokenPolicy: projection.TokenPolicy, BindingMode: projection.BindingMode}
	// Management-triggered attempts have already been reserved and must not consume another attempt.
	if record.Status == "queued" {
		state, err := repository.GetExecutionRecovery(attemptCtx, record.AccountID, record.AppID, record.ID)
		// Unknown queue ownership must not be replaced by a new automatic attempt.
		if err != nil {
			return
		}
		// Only explicitly queued control requests bypass automatic-retry policy.
		if state != nil && state.Ready {
			s.runQueuedRecovery(attemptCtx, repository, record, identity)
			return
		}
	}
	_, _ = s.executeRecoveryAttempt(attemptCtx, repository, record, identity, hex.EncodeToString(digest[:]), true)
}
