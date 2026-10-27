package api

import (
	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

// capabilityOriginIdentity carries Engine trigger provenance to children without reusing event reservations.
func capabilityOriginIdentity(spec capabilityRunSpec) auth.RuntimeIdentity {
	identity := spec.identity
	// Existing delegated chains are immutable and must survive further nested calls.
	if identity.RecoveryOrigin != nil {
		return identity
	}
	origin := &store.ExecutionRecoveryOrigin{AppID: identity.AppID, WebhookEventID: spec.webhookEventID}
	// Only the root schedule owns the occurrence; children retain provenance for authorization.
	if spec.schedule != nil {
		origin.ScheduleID = &spec.schedule.ID
	}
	identity.RecoveryOrigin = origin
	return identity
}

// attachedRecoveryOrigin copies ancestry so parallel children never mutate another invocation's authority.
func attachedRecoveryOrigin(identity auth.RuntimeIdentity, alias string, target uuid.UUID) *store.ExecutionRecoveryOrigin {
	origin := store.ExecutionRecoveryOrigin{AppID: identity.AppID}
	// A previously delegated or Engine-triggered caller preserves its original root authority.
	if identity.RecoveryOrigin != nil {
		origin = *identity.RecoveryOrigin
	}
	origin.Calls = append(append([]store.ExecutionRecoveryCall{}, origin.Calls...), store.ExecutionRecoveryCall{
		CallerID: identity.AppID, TargetID: target, Alias: alias,
	})
	return &origin
}

// recoveryOriginIdentity preserves delegated authority unless a fresh target token authorizes the rerun.
func recoveryOriginIdentity(source store.ExecutionResult, identity auth.RuntimeIdentity) auth.RuntimeIdentity {
	// A fresh direct token must not inherit the previous caller's delegation or trigger.
	if identity.TokenID != source.AppTokenID {
		identity.RecoveryOrigin = nil
		return identity
	}
	identity.RecoveryOrigin = source.RecoveryOrigin
	// Automation roots must retain trigger provenance when a recovered attempt starts a new child.
	if identity.RecoveryOrigin == nil && source.AppTokenID == uuid.Nil {
		identity.RecoveryOrigin = &store.ExecutionRecoveryOrigin{AppID: source.AppID, ScheduleID: source.SourceScheduleID, WebhookEventID: source.SourceWebhookEventID}
	}
	return identity
}
