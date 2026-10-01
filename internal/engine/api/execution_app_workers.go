package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/executionappvm"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const unifiedAppWarmRefresh = 10 * time.Second

// runUnifiedAppWorker isolates one invocation in the resident process for its promoted family version.
func (s *EngineGRPCServer) runUnifiedAppWorker(ctx context.Context, identity auth.RuntimeIdentity, bundle []byte, input json.RawMessage, host sandbox.CapabilityScriptHost, control sandbox.CapabilityDeterminism) (json.RawMessage, error) {
	// One live entitlement snapshot controls both residency and interpreter slots for this invocation.
	plan := entitlement.LiveEntitlement.Load()
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.runtime")
	defer span.End()
	span.SetAttributes(attribute.String("app.family_id", identity.AppFamilyID.String()), attribute.String("app.id", identity.AppID.String()))
	output, err := s.capabilityWorkerManager().Run(ctx, identity.AppFamilyID.String(), identity.AppID.String(), bundle, input, host, control,
		plan.UnifiedAppAlwaysOnEnabled, plan.MaxUnifiedAppConcurrency)
	// Worker exceptions can contain private values; only a closed phase vocabulary enters telemetry.
	if err != nil {
		span.SetAttributes(attribute.String("execution.failure_phase", telemetryFailurePhase(executionappvm.PrivateDiagnostic(err).Phase)))
		span.SetStatus(codes.Error, "execution_failed")
	} else {
		span.SetStatus(codes.Ok, "completed")
	}
	return output, err
}

// capabilityWorkerManager keeps one family worker registry across REST, MCP, and replay on this Engine server.
func (s *EngineGRPCServer) capabilityWorkerManager() *sandbox.CapabilityWorkerManager {
	s.capabilityWorkerOnce.Do(func() {
		s.capabilityWorkers = sandbox.NewCapabilityWorkerManager()
	})
	return s.capabilityWorkers
}

// StartUnifiedAppWarmReconciler follows the live plan and promoted family targets after Engine startup.
func (s *EngineGRPCServer) StartUnifiedAppWarmReconciler(ctx context.Context) {
	go s.runUnifiedAppWarmReconciler(ctx)
}

// runUnifiedAppWarmReconciler preloads current targets and observes later apply and plan changes.
func (s *EngineGRPCServer) runUnifiedAppWarmReconciler(ctx context.Context) {
	defer s.capabilityWorkerManager().Close()
	ticker := time.NewTicker(unifiedAppWarmRefresh)
	defer ticker.Stop()
	for {
		s.reconcileUnifiedAppWarmTargets(ctx)
		// Cancellation retires resident sandboxes independently of any individual HTTP request.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileUnifiedAppWarmTargets starts only the promoted versions admitted by the current plan.
func (s *EngineGRPCServer) reconcileUnifiedAppWarmTargets(ctx context.Context) {
	manager := s.capabilityWorkerManager()
	allowed := entitlement.LiveEntitlement.Load().UnifiedAppAlwaysOnEnabled
	manager.SetAlwaysOn(allowed)
	// Dev releases idle workers; a warm-target scan has no work until the plan enables residency.
	if !allowed {
		return
	}
	repository, ok := s.store.(store.UnifiedAppWarmStore)
	// A missing authoritative target query must never guess an active version from memory.
	if !ok {
		slog.WarnContext(ctx, "Unified App warm target store unavailable")
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	bundles, err := repository.ListWarmUnifiedAppBundles(readCtx)
	// A partial or stale target list must not evict a known worker during a transient SQL failure.
	if err != nil {
		slog.WarnContext(ctx, "Unified App warm target scan failed", "error", err)
		return
	}
	targets := make([]sandbox.CapabilityWarmTarget, 0, len(bundles))
	for _, bundle := range bundles {
		// The SQL result contains only one attached traffic version per family.
		targets = append(targets, sandbox.CapabilityWarmTarget{
			FamilyID: bundle.FamilyID.String(), AppID: bundle.AppID.String(), Bundle: []byte(bundle.BundleJS),
		})
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.unified_app.warm_reconcile")
	defer span.End()
	span.SetAttributes(attribute.Int("unified_app.warm_target_count", len(targets)))
	// A failed sandbox load stays observable and can be retried on the next bounded refresh.
	if err := manager.ReconcileWarmTargets(readCtx, targets); err != nil {
		span.SetStatus(codes.Error, "unified_app_warm_reconcile_failed")
		slog.WarnContext(ctx, "Unified App warm reconciliation failed", "error", err)
	}
}

// ConfigureUnifiedAppAdmission binds the resolved deployment policy before any transport or reconciler runs.
func (s *EngineGRPCServer) ConfigureUnifiedAppAdmission(policy config.UnifiedAppAdmissionConfig) error {
	// Invalid policies must not reserve the once guard or replace a working manager.
	if err := policy.Validate(); err != nil {
		return err
	}
	configured := false
	// Admission is immutable for the lifetime of this Engine; live replacement would split capacity accounting.
	s.capabilityWorkerOnce.Do(func() { s.capabilityWorkers = sandbox.NewCapabilityWorkerManagerWithConfig(policy); configured = true })
	// Callers must configure the shared REST/MCP executor before its first use.
	if !configured {
		return errors.New("Unified App worker manager is already initialized")
	}
	// Operators can verify effective deployment limits without logging configuration secrets.
	slog.Info("Unified App execution admission configured", "max_concurrency", policy.MaxConcurrency, "queue_capacity", policy.QueueCapacity, "queue_timeout_seconds", policy.QueueTimeoutSeconds)
	return nil
}
