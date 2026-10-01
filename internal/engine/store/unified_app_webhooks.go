package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UnifiedAppWebhookTarget carries only the promoted version's immutable event scope.
type UnifiedAppWebhookTarget struct {
	AccountID          uuid.UUID
	FamilyID           uuid.UUID
	AppID              uuid.UUID
	Version            string
	Attachment         string
	ScopeSchemaVersion int
	Selections         json.RawMessage
	CreatedAt          time.Time
}

// UnifiedAppWebhookStore keeps automatic execution authority and delivery reservations in Engine storage.
type UnifiedAppWebhookStore interface {
	ListUnifiedAppWebhookTargets(context.Context) ([]UnifiedAppWebhookTarget, error)
	GetUnifiedAppWebhookTarget(context.Context, uuid.UUID) (*UnifiedAppWebhookTarget, error)
	ReserveUnifiedAppWebhookExecution(context.Context, ExecutionResult) (uuid.UUID, bool, error)
	DeleteExpiredUnifiedAppWebhookDeliveries(context.Context, time.Time, int) (int64, error)
}

// The joined desired state and immutable selections are the same authorities used by SDK receivers.
const unifiedAppWebhookTargetQuery = `SELECT family.account_id, family.app_family_id, app.app_id,
	app.version, BTRIM(config.desired_state->>'webhook_attachment'), app.scope_schema_version,
	app.selections, app.created_at
	FROM fused_app_families family
	JOIN fused_apps app ON app.app_id=family.unified_active_app_id AND app.app_family_id=family.app_family_id
	JOIN fused_unified_app_bundles bundle ON bundle.app_id=app.app_id
	JOIN fused_config_states config ON config.config_key=app.config_key AND config.config_type='unified_app'
	WHERE family.kind='unified_app' AND family.archived_at IS NULL
	AND app.status IN ('active','deprecated')
	AND COALESCE(BTRIM(config.desired_state->>'webhook_attachment'),'') <> ''
	AND EXISTS (SELECT 1 FROM jsonb_array_elements(app.selections) selection
		WHERE jsonb_array_length(COALESCE(selection->'webhook_names','[]'::jsonb)) > 0
		OR selection->>'webhook_select_all'='true')`

// scanUnifiedAppWebhookTarget keeps batch discovery and per-delivery admission on identical scope fields.
func scanUnifiedAppWebhookTarget(row pgx.Row) (*UnifiedAppWebhookTarget, error) {
	var target UnifiedAppWebhookTarget
	err := row.Scan(&target.AccountID, &target.FamilyID, &target.AppID, &target.Version,
		&target.Attachment, &target.ScopeSchemaVersion, &target.Selections, &target.CreatedAt)
	return &target, err
}

// ListUnifiedAppWebhookTargets discovers eligible consumers in one set-based read without per-app config queries.
func (s *postgresStore) ListUnifiedAppWebhookTargets(ctx context.Context) ([]UnifiedAppWebhookTarget, error) {
	rows, err := s.db.Query(ctx, unifiedAppWebhookTargetQuery)
	// A failed snapshot must not retire working consumers.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []UnifiedAppWebhookTarget
	// SQL already filters eligibility; each row is a complete immutable subscription description.
	for rows.Next() {
		target, err := scanUnifiedAppWebhookTarget(rows)
		// Partial snapshots cannot safely reconcile lifecycle changes.
		if err != nil {
			return nil, err
		}
		targets = append(targets, *target)
	}
	return targets, rows.Err()
}

// GetUnifiedAppWebhookTarget rechecks promotion and deactivation before each event can acquire execution authority.
func (s *postgresStore) GetUnifiedAppWebhookTarget(ctx context.Context, familyID uuid.UUID) (*UnifiedAppWebhookTarget, error) {
	target, err := scanUnifiedAppWebhookTarget(s.db.QueryRow(ctx, unifiedAppWebhookTargetQuery+` AND family.app_family_id=$1`, familyID))
	// Removed attachments and deactivated families have no automatic execution target.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppRuntimeNotFound
	}
	return target, err
}

// ReserveUnifiedAppWebhookExecution commits the delivery fence and accepted result together before provider effects.
func (s *postgresStore) ReserveUnifiedAppWebhookExecution(ctx context.Context, record ExecutionResult) (uuid.UUID, bool, error) {
	// Automatic execution never impersonates a caller token or a rerun request.
	if record.SourceWebhookEventID == "" || record.Mode != "live" || record.AppTokenID != uuid.Nil {
		return uuid.Nil, false, ErrExecutionResultInvalid
	}
	tx, err := s.db.Begin(ctx)
	// Without a transaction, a delivery fence could survive a failed result reservation.
	if err != nil {
		return uuid.Nil, false, err
	}
	defer tx.Rollback(ctx)
	// Locking the family serializes admission with promotion and hard deactivation.
	var activeID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT unified_active_app_id FROM fused_app_families
		WHERE app_family_id=$1 AND account_id=$2 AND kind='unified_app' AND archived_at IS NULL
		FOR SHARE`, record.AppFamilyID, record.AccountID).Scan(&activeID)
	// A stale subscriber retries through the newly promoted target instead of running retired code.
	if err != nil {
		return uuid.Nil, false, err
	}
	if activeID != record.AppID {
		return uuid.Nil, false, ErrAppDeactivated
	}
	id, created, err := reserveUnifiedWebhookDelivery(ctx, tx, record)
	// An existing delivery already owns its result, even after result-body retention has elapsed.
	if err != nil || !created {
		return id, false, err
	}
	writer := &postgresStore{db: workspaceTransaction{Tx: tx}}
	// Shared validation and insertion retain the normal execution-record contract.
	if err := writer.CreateExecutionResult(ctx, record); err != nil {
		return uuid.Nil, false, err
	}
	return record.ID, true, tx.Commit(ctx)
}

// reserveUnifiedWebhookDelivery keeps a payload-free fence longer than the existing 30-day WEBHOOKS retention.
func reserveUnifiedWebhookDelivery(ctx context.Context, tx pgx.Tx, record ExecutionResult) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO fused_unified_app_webhook_deliveries
		(account_id, app_family_id, event_id, execution_id, expires_at)
		VALUES ($1,$2,$3,$4,NOW()+INTERVAL '32 days')
		ON CONFLICT (app_family_id,event_id) DO NOTHING RETURNING execution_id`,
		record.AccountID, record.AppFamilyID, record.SourceWebhookEventID, record.ID).Scan(&id)
	// A successful insert reserves exactly one run across versions and Engine replicas.
	if err == nil {
		return id, true, nil
	}
	// Only the known uniqueness conflict can be treated as a previous admission.
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, err
	}
	err = tx.QueryRow(ctx, `SELECT execution_id FROM fused_unified_app_webhook_deliveries
		WHERE account_id=$1 AND app_family_id=$2 AND event_id=$3`, record.AccountID, record.AppFamilyID, record.SourceWebhookEventID).Scan(&id)
	return id, false, err
}

// DeleteExpiredUnifiedAppWebhookDeliveries bounds retention work independently of sensitive result bodies.
func (s *postgresStore) DeleteExpiredUnifiedAppWebhookDeliveries(ctx context.Context, before time.Time, limit int) (int64, error) {
	// Bounded batches prevent a reconnecting worker from monopolizing storage.
	if limit < 1 || limit > 1000 {
		return 0, ErrExecutionResultInvalid
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_unified_app_webhook_deliveries WHERE (app_family_id,event_id) IN
		(SELECT app_family_id,event_id FROM fused_unified_app_webhook_deliveries WHERE expires_at<$1
		ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, limit)
	return tag.RowsAffected(), err
}

// ListUnifiedAppWebhookTargets bypasses caches so consumer reconciliation follows authoritative traffic state.
func (s *cachedStore) ListUnifiedAppWebhookTargets(ctx context.Context) ([]UnifiedAppWebhookTarget, error) {
	repository, ok := s.Store.(UnifiedAppWebhookStore)
	// A partial store must not manufacture an empty successful snapshot.
	if !ok {
		return nil, errors.New("unified app webhook store unavailable")
	}
	return repository.ListUnifiedAppWebhookTargets(ctx)
}

// GetUnifiedAppWebhookTarget prevents cached version state from admitting webhook work after promotion.
func (s *cachedStore) GetUnifiedAppWebhookTarget(ctx context.Context, familyID uuid.UUID) (*UnifiedAppWebhookTarget, error) {
	repository, ok := s.Store.(UnifiedAppWebhookStore)
	// Delivery requires the same authoritative repository as reconciliation.
	if !ok {
		return nil, errors.New("unified app webhook store unavailable")
	}
	return repository.GetUnifiedAppWebhookTarget(ctx, familyID)
}

// ReserveUnifiedAppWebhookExecution delegates atomic deduplication to the database shared by Engine replicas.
func (s *cachedStore) ReserveUnifiedAppWebhookExecution(ctx context.Context, record ExecutionResult) (uuid.UUID, bool, error) {
	repository, ok := s.Store.(UnifiedAppWebhookStore)
	// A cache cannot establish durable ownership of an event.
	if !ok {
		return uuid.Nil, false, errors.New("unified app webhook store unavailable")
	}
	return repository.ReserveUnifiedAppWebhookExecution(ctx, record)
}

// DeleteExpiredUnifiedAppWebhookDeliveries keeps lightweight delivery fences off the runtime cache path.
func (s *cachedStore) DeleteExpiredUnifiedAppWebhookDeliveries(ctx context.Context, before time.Time, limit int) (int64, error) {
	repository, ok := s.Store.(UnifiedAppWebhookStore)
	// Unsupported persistence cannot claim successful retention cleanup.
	if !ok {
		return 0, errors.New("unified app webhook store unavailable")
	}
	return repository.DeleteExpiredUnifiedAppWebhookDeliveries(ctx, before, limit)
}
