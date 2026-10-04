package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

// WorkspaceWebhookListing adds persisted service identity without Registry enrichment.
type WorkspaceWebhookListing struct {
	WorkspaceWebhook
	ServiceName string
	ServiceRef  string
}

// WorkspaceWebhookFilter narrows persisted registrations without fetching a service catalogue.
type WorkspaceWebhookFilter struct {
	ServiceID *uuid.UUID
	Search    string
}

const authorizedWebhookFromSQL = ` FROM fused_workspace_webhooks w
 JOIN fused_workspace_services s ON s.service_id = w.service_id
 WHERE ($1::boolean OR w.service_id = ANY($2::uuid[]))
 AND ($3::uuid IS NULL OR w.service_id = $3)
 AND ($4::text = '' OR strpos(lower(w.label || ' ' || w.slug || ' ' || w.callback_url || ' ' ||
     COALESCE(s.service_name, '') || ' ' || COALESCE(s.service_slug, '')), lower($4)) > 0)
 AND EXISTS (SELECT 1 FROM fused_workspace_service_versions v
             WHERE v.service_id = s.service_id AND v.status <> 'deprecated')`

// ListAuthorizedWorkspaceWebhookPage filters registrations before totals and pagination, without per-service queries.
func (s *postgresStore) ListAuthorizedWorkspaceWebhookPage(ctx context.Context, scope accesscontrol.AuthorizedScope, filter WorkspaceWebhookFilter, limit, offset int) ([]WorkspaceWebhookListing, int, error) {
	// An empty resource grant must never become an unrestricted registration listing.
	if !scope.All && len(scope.IDs) == 0 {
		return nil, 0, nil
	}
	var total int
	// Keep totals accurate even when the requested page is beyond the last registration.
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*)`+authorizedWebhookFromSQL, scope.All, scope.IDs, filter.ServiceID, filter.Search).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count authorized webhooks: %w", err)
	}
	// Empty workspaces need no second read and no Registry availability.
	if total == 0 {
		return nil, 0, nil
	}
	rows, err := s.db.Query(ctx, `SELECT w.service_id, COALESCE(s.service_name, ''), COALESCE(s.service_slug, ''),
	 w.label, w.slug, w.callback_url, w.relay_config, w.secret_bucket_id, w.secret_ref, w.created_at`+authorizedWebhookFromSQL+`
	 ORDER BY s.service_name, w.service_id, w.label, w.id LIMIT $5 OFFSET $6`, scope.All, scope.IDs, filter.ServiceID, filter.Search, limit, offset)
	// Storage failures are surfaced instead of presenting a misleading empty catalogue.
	if err != nil {
		return nil, 0, fmt.Errorf("list authorized webhooks: %w", err)
	}
	defer rows.Close()
	items := make([]WorkspaceWebhookListing, 0)
	// Only projection inputs are read; verification policy and provider credentials are unnecessary here.
	for rows.Next() {
		var item WorkspaceWebhookListing
		// An invalid row must fail the page rather than silently omit a registration.
		if err := rows.Scan(&item.ServiceID, &item.ServiceName, &item.ServiceRef, &item.Label, &item.Slug,
			&item.CallbackURL, &item.RelayConfig, &item.SecretBucketID, &item.SecretRef, &item.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan authorized webhook: %w", err)
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}
