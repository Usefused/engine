package store

import (
	"context"

	"github.com/google/uuid"
)

// ExecutionRecoveryOrigin retains credential-free authority across nested calls and process restarts.
// Only Engine constructs it; request bodies never supply this metadata.
type ExecutionRecoveryOrigin struct {
	AppID          uuid.UUID               `json:"appId"`
	ScheduleID     *uuid.UUID              `json:"scheduleId,omitempty"`
	WebhookEventID string                  `json:"webhookEventId,omitempty"`
	Calls          []ExecutionRecoveryCall `json:"calls"`
}

// ExecutionRecoveryCall pins one authored delegation rather than inferring access from a token's family.
type ExecutionRecoveryCall struct {
	CallerID uuid.UUID `json:"callerId"`
	TargetID uuid.UUID `json:"targetId"`
	Alias    string    `json:"alias"`
}

// HasAutomation identifies Engine-owned origins without borrowing a child's event-delivery identity.
func (origin *ExecutionRecoveryOrigin) HasAutomation() bool {
	return origin != nil && (origin.ScheduleID != nil || origin.WebhookEventID != "")
}

// validRecoveryOrigin bounds persisted provenance and ensures the chain reaches this exact execution.
func validRecoveryOrigin(record ExecutionResult) bool {
	origin := record.RecoveryOrigin
	// Direct callers retain the original token/trigger representation.
	if origin == nil {
		return true
	}
	// Depth remains bounded even when continuation starts in another Engine process.
	if origin.AppID == uuid.Nil || len(origin.Calls) < 1 || len(origin.Calls) > 8 {
		return false
	}
	return validRecoveryCalls(origin, record.AppID)
}

// validRecoveryCalls rejects discontinuities and cycles before durable authority is admitted.
func validRecoveryCalls(origin *ExecutionRecoveryOrigin, target uuid.UUID) bool {
	caller := origin.AppID
	seen := map[uuid.UUID]bool{caller: true}
	// Every edge must be contiguous and must not recreate a cycle on recovery.
	for _, call := range origin.Calls {
		// A malformed edge cannot skip an intermediate caller or regain a visited app.
		if call.CallerID != caller || call.TargetID == uuid.Nil || seen[call.TargetID] || len(call.Alias) < 1 || len(call.Alias) > 64 {
			return false
		}
		seen[call.TargetID] = true
		caller = call.TargetID
	}
	return caller == target
}

// recoveryOriginAuthority rechecks the root token, every immutable binding, and current traffic in one bounded SQL query.
// The surrounding query supplies result alias r; no credential material is persisted or reconstructed.
const recoveryOriginAuthority = `EXISTS (
 SELECT 1 FROM fused_apps root JOIN fused_app_families root_family ON root_family.app_family_id=root.app_family_id
 LEFT JOIN fused_app_tokens origin_token ON origin_token.id=r.app_token_id AND origin_token.app_family_id=root.app_family_id
 WHERE root.app_id=COALESCE((r.recovery_origin->>'appId')::uuid,r.app_id)
 AND root.account_id=r.account_id AND root_family.account_id=r.account_id AND root_family.archived_at IS NULL
 AND root.status IN ('active','deprecated')
 AND (root_family.kind<>'unified_app' OR root_family.unified_active_app_id=root.app_id)
 AND ((r.app_token_id='00000000-0000-0000-0000-000000000000' AND
   (r.source_schedule_id IS NOT NULL OR r.source_webhook_event_id IS NOT NULL OR
    r.recovery_origin->>'scheduleId' IS NOT NULL OR r.recovery_origin->>'webhookEventId' IS NOT NULL))
 OR (origin_token.id IS NOT NULL AND (origin_token.expires_at IS NULL OR origin_token.expires_at>clock_timestamp())
   AND (r.recovery_origin IS NULL OR origin_token.allow_all OR
     ('unified_app:'||(r.recovery_origin->'calls'->0->>'alias'))=ANY(origin_token.allowed_operations))))
 AND NOT EXISTS (
   SELECT 1 FROM jsonb_to_recordset(COALESCE(r.recovery_origin->'calls','[]'::jsonb)) edge("callerId" uuid,"targetId" uuid,alias text)
   WHERE NOT EXISTS (
     SELECT 1 FROM fused_apps caller JOIN fused_app_families caller_family ON caller_family.app_family_id=caller.app_family_id
     JOIN fused_apps target ON target.app_id=edge."targetId" AND target.account_id=r.account_id
     JOIN fused_app_families target_family ON target_family.app_family_id=target.app_family_id AND target_family.account_id=r.account_id
     JOIN LATERAL (
       SELECT applied.resolved_payload FROM fused_config_plans applied
       WHERE applied.config_key=caller.config_key AND applied.source_hash=caller.source_hash AND applied.status='applied'
       AND NOT COALESCE((applied.resolved_payload->>'noop')::boolean,false)
       ORDER BY applied.applied_at DESC,applied.created_at DESC LIMIT 1
     ) plan ON true
     WHERE caller.app_id=edge."callerId" AND caller.account_id=r.account_id AND caller_family.account_id=r.account_id
     AND caller.status IN ('active','deprecated') AND caller_family.archived_at IS NULL
     AND (caller_family.kind<>'unified_app' OR caller_family.unified_active_app_id=caller.app_id)
     AND target.status IN ('active','deprecated') AND target_family.archived_at IS NULL
     AND target_family.kind='unified_app' AND target_family.unified_active_app_id=target.app_id
     AND EXISTS (SELECT 1 FROM jsonb_array_elements(plan.resolved_payload->'unified_apps') binding
       WHERE binding->>'alias'=edge.alias AND (binding->>'app_id')::uuid=target.app_id
       AND binding->>'source_hash'=target.source_hash AND binding->>'bundle_digest'=target.bundle_digest)
   )
 ))`

// validateRecoveryOrigin admits automatic work only when its durable caller chain still has live authority.
func (s *postgresStore) validateRecoveryOrigin(ctx context.Context, record ExecutionResult) error {
	// Manual policy remains explicitly authorized at the rerun boundary.
	if record.Recovery.Mode != "automatic" {
		return nil
	}
	// Reject malformed internal provenance before SQL evaluates any supplied UUIDs.
	if !validRecoveryOrigin(record) {
		return ErrExecutionRecoveryOrigin
	}
	var allowed bool
	err := s.db.QueryRow(ctx, `SELECT `+recoveryOriginAuthority+` FROM (SELECT $1::uuid account_id,$2::uuid app_id,
      $3::uuid app_token_id,$4::uuid source_schedule_id,NULLIF($5::text,'') source_webhook_event_id,$6::jsonb recovery_origin) r`,
		record.AccountID, record.AppID, record.AppTokenID, record.SourceScheduleID, record.SourceWebhookEventID, record.RecoveryOrigin).Scan(&allowed)
	// Database uncertainty cannot authorize autonomous effects.
	if err != nil {
		return err
	}
	// Unsupported provenance and revoked authority both fail before initial execution.
	if !allowed {
		return ErrExecutionRecoveryOrigin
	}
	return nil
}
