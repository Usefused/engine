package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// WebhookConsumptionRecorder records per-consumer webhook delivery timestamps without altering broker acknowledgement.
type WebhookConsumptionRecorder interface {
	RecordWebhookConsumption(context.Context, models.WebhookConsumption) error
}

// WebhookConsumptionStore persists, queries, and expires per-consumer webhook consumption receipts.
type WebhookConsumptionStore interface {
	WebhookConsumptionRecorder
	ListWebhookConsumptions(context.Context, uuid.UUID, []uuid.UUID) (map[uuid.UUID][]models.WebhookConsumption, error)
	DeleteExpiredWebhookConsumptions(context.Context, time.Time, int) (int64, error)
}

// validWebhookConsumerKind enforces the closed set of supported Fused webhook consumer categories.
func validWebhookConsumerKind(kind string) bool {
	// Only recognized Fused consumer transports may enter canonical webhook receipts.
	switch kind {
	case "sdk", "mcp", "unified_app", "relay":
		return true
	default:
		return false
	}
}

// RecordWebhookConsumption stores one idempotent consumption fact for a webhook event without blocking on async ingress flushes.
func (s *postgresStore) RecordWebhookConsumption(ctx context.Context, record models.WebhookConsumption) error {
	record.ConsumerKind = strings.TrimSpace(record.ConsumerKind)
	record.ConsumerKey = strings.TrimSpace(record.ConsumerKey)
	// Reject malformed consumer identity so tenant-scoped receipts never gain orphan or unbounded keys.
	if record.EventID == uuid.Nil || record.AccountID == uuid.Nil || !validWebhookConsumerKind(record.ConsumerKind) || record.ConsumerKey == "" || len(record.ConsumerKey) > 512 {
		return ErrExecutionResultInvalid
	}
	// Preserve caller-supplied observation timestamps while supplying a UTC fallback for direct recorders.
	if record.ConsumedAt.IsZero() {
		record.ConsumedAt = time.Now().UTC()
	}
	_, err := s.db.Exec(ctx, `INSERT INTO fused_webhook_consumptions (
			event_id, account_id, consumer_kind, consumer_key, app_family_id, app_id,
			app_version, consumer_name, receiver_name, consumed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (event_id, consumer_kind, consumer_key) DO NOTHING`,
		record.EventID, record.AccountID, record.ConsumerKind, record.ConsumerKey,
		record.AppFamilyID, record.AppID, strings.TrimSpace(record.AppVersion),
		strings.TrimSpace(record.ConsumerName), strings.TrimSpace(record.ReceiverName),
		record.ConsumedAt.UTC())
	return err
}

// ListWebhookConsumptions batch-loads consumer metadata for one receipt page within the authenticated account boundary.
func (s *postgresStore) ListWebhookConsumptions(ctx context.Context, accountID uuid.UUID, eventIDs []uuid.UUID) (map[uuid.UUID][]models.WebhookConsumption, error) {
	result := make(map[uuid.UUID][]models.WebhookConsumption)
	// Empty receipt pages avoid an unnecessary array-scan round trip.
	if accountID == uuid.Nil || len(eventIDs) == 0 {
		return result, nil
	}
	rows, err := s.db.Query(ctx, `SELECT c.event_id, c.account_id, c.consumer_kind, c.consumer_key,
			c.app_family_id, c.app_id,
			COALESCE(NULLIF(a.version, ''), c.app_version, '') AS app_version,
			COALESCE(NULLIF(f.display_name, ''), c.consumer_name, '') AS consumer_name,
			c.receiver_name, c.consumed_at
		FROM fused_webhook_consumptions c
		LEFT JOIN fused_app_families f ON f.app_family_id = c.app_family_id AND f.account_id = c.account_id
		LEFT JOIN fused_apps a ON a.app_id = c.app_id AND a.account_id = c.account_id
		WHERE c.account_id = $1 AND c.event_id = ANY($2)
		ORDER BY c.consumed_at ASC, c.consumer_kind ASC, c.consumer_key ASC`,
		accountID, eventIDs)
	// Storage failures remain explicit instead of pretending no consumer received the webhook.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item models.WebhookConsumption
		// Partial row scans must abort rather than returning truncated consumer lists.
		if err := rows.Scan(&item.EventID, &item.AccountID, &item.ConsumerKind, &item.ConsumerKey,
			&item.AppFamilyID, &item.AppID, &item.AppVersion, &item.ConsumerName, &item.ReceiverName, &item.ConsumedAt); err != nil {
			return nil, err
		}
		result[item.EventID] = append(result[item.EventID], item)
	}
	return result, rows.Err()
}

// attachWebhookConsumptions hydrates per-consumer history and historical Unified App fences for one receipt page.
func (s *postgresStore) attachWebhookConsumptions(ctx context.Context, accountID uuid.UUID, events []models.WebhookEvent) error {
	// Skip hydration when the page has no receipts to enrich.
	if len(events) == 0 {
		return nil
	}
	eventIDs := make([]uuid.UUID, 0, len(events))
	msgIDs := make([]string, 0, len(events))
	msgToEvent := make(map[string]uuid.UUID, len(events))
	for _, event := range events {
		eventIDs = append(eventIDs, event.ID)
		// Historical Unified App deliveries key by provider message ID rather than receipt UUID.
		if event.MsgID != "" {
			msgIDs = append(msgIDs, event.MsgID)
			msgToEvent[event.MsgID] = event.ID
		}
	}
	byEvent, err := s.ListWebhookConsumptions(ctx, accountID, eventIDs)
	// Do not mask consumption lookup errors as unconsumed webhooks.
	if err != nil {
		return err
	}
	// Backfill historical Unified App deliveries recorded before fused_webhook_consumptions existed.
	if err := s.mergeHistoricalUnifiedWebhookConsumptions(ctx, accountID, msgIDs, msgToEvent, byEvent); err != nil {
		return err
	}
	for i := range events {
		items := byEvent[events[i].ID]
		// Deterministic chronological ordering keeps UI summaries stable across queries.
		if len(items) > 1 {
			sort.Slice(items, func(left, right int) bool {
				if !items[left].ConsumedAt.Equal(items[right].ConsumedAt) {
					return items[left].ConsumedAt.Before(items[right].ConsumedAt)
				}
				if items[left].ConsumerKind != items[right].ConsumerKind {
					return items[left].ConsumerKind < items[right].ConsumerKind
				}
				return items[left].ConsumerKey < items[right].ConsumerKey
			})
		}
		events[i].Consumptions = items
	}
	return nil
}

// mergeHistoricalUnifiedWebhookConsumptions adds existing Unified App delivery fences not already in fused_webhook_consumptions.
func (s *postgresStore) mergeHistoricalUnifiedWebhookConsumptions(ctx context.Context, accountID uuid.UUID, msgIDs []string, msgToEvent map[string]uuid.UUID, byEvent map[uuid.UUID][]models.WebhookConsumption) error {
	// Receipts without provider message IDs cannot match Unified App delivery fences.
	if len(msgIDs) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `SELECT d.event_id, d.app_family_id, r.app_id,
			COALESCE(a.version, '') AS app_version,
			COALESCE(f.display_name, '') AS consumer_name,
			r.created_at
		FROM fused_unified_app_webhook_deliveries d
		JOIN fused_unified_app_results r ON r.id = d.execution_id AND r.account_id = d.account_id
		LEFT JOIN fused_app_families f ON f.app_family_id = d.app_family_id AND f.account_id = d.account_id
		LEFT JOIN fused_apps a ON a.app_id = r.app_id AND a.account_id = d.account_id
		WHERE d.account_id = $1 AND d.event_id = ANY($2)
		ORDER BY r.created_at ASC`,
		accountID, msgIDs)
	// Historical backfill errors propagate so receipts never misreport Unified App consumption as absent.
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			msgID        string
			familyID     uuid.UUID
			appID        uuid.UUID
			appVersion   string
			consumerName string
			consumedAt   time.Time
		)
		// Abort on malformed scan results rather than attaching partial identity.
		if err := rows.Scan(&msgID, &familyID, &appID, &appVersion, &consumerName, &consumedAt); err != nil {
			return err
		}
		eventID, ok := msgToEvent[msgID]
		// Ignore unmatched rows defensively even though SQL filtered by msgIDs.
		if !ok {
			continue
		}
		// Deduplicate against explicit fused_webhook_consumptions rows for the same Unified App family.
		if hasUnifiedConsumption(byEvent[eventID], familyID) {
			continue
		}
		familyCopy, appCopy := familyID, appID
		byEvent[eventID] = append(byEvent[eventID], models.WebhookConsumption{
			EventID:      eventID,
			AccountID:    accountID,
			ConsumerKind: "unified_app",
			ConsumerKey:  familyID.String(),
			AppFamilyID:  &familyCopy,
			AppID:        &appCopy,
			AppVersion:   appVersion,
			ConsumerName: consumerName,
			ConsumedAt:   consumedAt,
		})
	}
	return rows.Err()
}

// hasUnifiedConsumption checks whether a Unified App family is already present in an event's consumption list.
func hasUnifiedConsumption(items []models.WebhookConsumption, familyID uuid.UUID) bool {
	for _, item := range items {
		// Match either by family UUID or by the canonical family consumer key.
		if item.ConsumerKind == "unified_app" && ((item.AppFamilyID != nil && *item.AppFamilyID == familyID) || item.ConsumerKey == familyID.String()) {
			return true
		}
	}
	return false
}

// DeleteExpiredWebhookConsumptions removes aged consumption records in bounded batches aligned with execution retention.
func (s *postgresStore) DeleteExpiredWebhookConsumptions(ctx context.Context, before time.Time, limit int) (int64, error) {
	// Reject invalid batch limits so retention cleanup cannot run an unbounded table delete.
	if limit < 1 || limit > 10000 {
		return 0, ErrExecutionResultInvalid
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_webhook_consumptions WHERE ctid IN (
			SELECT ctid FROM fused_webhook_consumptions
			WHERE consumed_at < $1
			ORDER BY consumed_at ASC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)`, before, limit)
	return tag.RowsAffected(), err
}

// RecordWebhookConsumption delegates consumption writes directly to storage without touching runtime caches.
func (s *cachedStore) RecordWebhookConsumption(ctx context.Context, record models.WebhookConsumption) error {
	recorder, ok := s.Store.(WebhookConsumptionRecorder)
	// Missing persistence support surfaces explicitly instead of silently dropping consumption facts.
	if !ok {
		return errors.New("webhook consumption store unavailable")
	}
	return recorder.RecordWebhookConsumption(ctx, record)
}

// ListWebhookConsumptions bypasses runtime caches so newly acknowledged consumers appear immediately on refresh.
func (s *cachedStore) ListWebhookConsumptions(ctx context.Context, accountID uuid.UUID, eventIDs []uuid.UUID) (map[uuid.UUID][]models.WebhookConsumption, error) {
	reader, ok := s.Store.(WebhookConsumptionStore)
	// Unsupported underlying stores fail explicitly instead of reporting unconsumed events.
	if !ok {
		return nil, fmt.Errorf("webhook consumption store unavailable")
	}
	return reader.ListWebhookConsumptions(ctx, accountID, eventIDs)
}

// DeleteExpiredWebhookConsumptions delegates retention cleanup to the underlying persistent store.
func (s *cachedStore) DeleteExpiredWebhookConsumptions(ctx context.Context, before time.Time, limit int) (int64, error) {
	cleaner, ok := s.Store.(WebhookConsumptionStore)
	// Unsupported delegates report their absence so retention workers do not mistake them for drained batches.
	if !ok {
		return 0, errors.New("webhook consumption store unavailable")
	}
	return cleaner.DeleteExpiredWebhookConsumptions(ctx, before, limit)
}
