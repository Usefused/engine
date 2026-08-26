package executionevent

import (
	"context"
	"errors"

	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type unifiedChildKey struct{}
type unifiedChild struct {
	parent        uuid.UUID
	target, phase string
}

// WithUnifiedChild carries server-owned receipt correlation independently of optional tracing.
func WithUnifiedChild(ctx context.Context, parent uuid.UUID, target, phase string) context.Context {
	return context.WithValue(ctx, unifiedChildKey{}, unifiedChild{parent, target, phase})
}

// AttachUnifiedChild decorates the existing physical receipt, never creating another provider event.
func AttachUnifiedChild(ctx context.Context, event *models.EngineExecutionEvent) {
	child, ok := ctx.Value(unifiedChildKey{}).(unifiedChild)
	// Ordinary executions and tests without a logical owner retain their existing identity.
	if !ok || child.parent == uuid.Nil {
		return
	}
	event.ParentExecutionID, event.UnifiedTarget, event.ExecutionPhase = child.parent, child.target, child.phase
}

// Kind normalizes historical events while leaving the logical/provider distinction explicit in new storage.
func Kind(event models.EngineExecutionEvent) string {
	// Legacy producers and non-Unified transports omitted the discriminator.
	if event.ExecutionKind == "" {
		return "physical"
	}
	return event.ExecutionKind
}

// ValidateUnifiedMetadata is shared by producer and persistence so durable replay
// cannot bypass the bounded, metadata-only parent/child receipt contract.
func ValidateUnifiedMetadata(event models.EngineExecutionEvent) error {
	// There are no implicit logical kinds: legacy absence means physical only.
	if Kind(event) != "physical" && Kind(event) != "unified" {
		return errors.New("invalid execution kind")
	}
	// Logical receipts never carry provider identity, provider usage, or another parent.
	if Kind(event) == "unified" {
		return validateUnifiedParent(event)
	}
	// Provider receipts must not acquire a second copy of the logical step list.
	if len(event.UnifiedSteps) != 0 {
		return errors.New("physical receipt cannot contain unified steps")
	}
	// Standalone historical receipts omit all parent correlation fields together.
	if event.ParentExecutionID == uuid.Nil {
		if event.UnifiedTarget != "" || event.ExecutionPhase != "" {
			return errors.New("child receipt requires a parent")
		}
		return nil
	}
	return validateUnifiedStep(models.UnifiedExecutionStep{Target: event.UnifiedTarget, Phase: event.ExecutionPhase, Status: "success"})
}

// validateUnifiedParent limits authored diagnostic metadata while enforcing that
// provider counters and timing never become attributed to the logical envelope.
func validateUnifiedParent(event models.EngineExecutionEvent) error {
	// These fields have provider semantics and would corrupt accounting if copied onto a parent.
	if hasUnifiedPhysicalAccounting(event) {
		return errors.New("unified receipt cannot contain physical accounting")
	}
	// At most sixteen forward and sixteen rollback steps can be admitted by the scheduler.
	if len(event.UnifiedSteps) > 32 || !unified.ValidPublicName(event.EndpointName, 256) {
		return errors.New("unified receipt metadata exceeds bounds")
	}
	seen := make(map[string]bool, len(event.UnifiedSteps))
	// Each phase/target pair represents one logical outcome, not retries.
	for _, step := range event.UnifiedSteps {
		if err := validateUnifiedStep(step); err != nil {
			return err
		}
		key := step.Phase + ":" + step.Target
		// Duplicate steps would make receipt-to-child navigation ambiguous.
		if seen[key] {
			return errors.New("duplicate unified receipt step")
		}
		seen[key] = true
	}
	return nil
}

// hasUnifiedPhysicalAccounting keeps provider-specific invariants separate from step validation.
func hasUnifiedPhysicalAccounting(event models.EngineExecutionEvent) bool {
	return event.ParentExecutionID != uuid.Nil || event.ServiceID != uuid.Nil || event.OperationID != uuid.Nil || event.ProviderLatencyMs != nil || event.AttemptCount != 0
}

// validateUnifiedStep admits only bounded diagnostics, never arbitrary provider messages.
func validateUnifiedStep(step models.UnifiedExecutionStep) error {
	// Target names come from the authorized definition but still need a durable size bound.
	if !unified.ValidPublicName(step.Target, 253) || len(step.ErrorCode) > 128 {
		return errors.New("invalid unified step metadata")
	}
	// Phase is server-owned and explicitly separates compensation from forward work.
	if step.Phase != "forward" && step.Phase != "rollback" {
		return errors.New("invalid unified execution phase")
	}
	// These are the scheduler's diagnostics, not the canonical receipt success/failed vocabulary.
	if step.Status != "success" && step.Status != "error" && step.Status != "skipped" {
		return errors.New("invalid unified step status")
	}
	return nil
}
