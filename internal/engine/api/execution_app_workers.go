package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const executionAppWarmRefresh = 10 * time.Second

// runExecutionAppWorker isolates one invocation in the resident process for its promoted family version.
func (s *EngineGRPCServer) runExecutionAppWorker(ctx context.Context, identity auth.RuntimeIdentity, bundle []byte, input json.RawMessage, host sandbox.CapabilityScriptHost, control sandbox.CapabilityDeterminism) (json.RawMessage, error) {
	// One live entitlement snapshot controls both residency and interpreter slots for this invocation.
	plan := entitlement.LiveEntitlement.Load()
	return s.capabilityWorkerManager().Run(ctx, identity.AppFamilyID.String(), identity.AppID.String(), bundle, input, host, control,
		plan.ExecutionAppAlwaysOnEnabled, plan.MaxExecutionAppConcurrency)
}

// capabilityWorkerManager keeps one process registry across REST, MCP, and replay on this Engine server.
func (s *EngineGRPCServer) capabilityWorkerManager() *sandbox.CapabilityWorkerManager {
	s.capabilityWorkerOnce.Do(func() {
		s.capabilityWorkers = sandbox.NewCapabilityWorkerManager()
	})
	return s.capabilityWorkers
}

// StartExecutionAppWarmReconciler follows the live plan and promoted family targets after Engine startup.
func (s *EngineGRPCServer) StartExecutionAppWarmReconciler(ctx context.Context) {
	go s.runExecutionAppWarmReconciler(ctx)
}

// runExecutionAppWarmReconciler preloads current targets and observes later apply and plan changes.
func (s *EngineGRPCServer) runExecutionAppWarmReconciler(ctx context.Context) {
	defer s.capabilityWorkerManager().Close()
	ticker := time.NewTicker(executionAppWarmRefresh)
	defer ticker.Stop()
	for {
		s.reconcileExecutionAppWarmTargets(ctx)
		// Cancellation must stop children instead of leaving their process lifetime tied to an HTTP request.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcileExecutionAppWarmTargets starts only the promoted versions admitted by the current plan.
func (s *EngineGRPCServer) reconcileExecutionAppWarmTargets(ctx context.Context) {
	manager := s.capabilityWorkerManager()
	allowed := entitlement.LiveEntitlement.Load().ExecutionAppAlwaysOnEnabled
	manager.SetAlwaysOn(allowed)
	// Dev releases idle workers; a warm-target scan has no work until the plan enables residency.
	if !allowed {
		return
	}
	repository, ok := s.store.(store.ExecutionAppWarmStore)
	// A missing authoritative target query must never guess an active version from memory.
	if !ok {
		slog.WarnContext(ctx, "Execution App warm target store unavailable")
		return
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	bundles, err := repository.ListWarmExecutionAppBundles(readCtx)
	// A partial or stale target list must not evict a known worker during a transient SQL failure.
	if err != nil {
		slog.WarnContext(ctx, "Execution App warm target scan failed", "error", err)
		return
	}
	targets := make([]sandbox.CapabilityWarmTarget, 0, len(bundles))
	for _, bundle := range bundles {
		// The SQL result contains only one attached traffic version per family.
		targets = append(targets, sandbox.CapabilityWarmTarget{
			FamilyID: bundle.FamilyID.String(), AppID: bundle.AppID.String(), Bundle: []byte(bundle.BundleJS),
		})
	}
	_, span := otel.Tracer("engine").Start(ctx, "engine.execution_app.warm_reconcile")
	defer span.End()
	span.SetAttributes(attribute.Int("execution_app.warm_target_count", len(targets)))
	// A failed process load stays observable and can be retried on the next bounded refresh.
	if err := manager.ReconcileWarmTargets(readCtx, targets); err != nil {
		span.SetStatus(codes.Error, "execution_app_warm_reconcile_failed")
		slog.WarnContext(ctx, "Execution App warm reconciliation failed", "error", err)
	}
}
