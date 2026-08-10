package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

type ServiceConsumer struct {
	AppID            uuid.UUID
	Name             string
	Version          string
	Kind             AppKind
	Status           AppStatus
	ServiceVersionID uuid.UUID
	SelectAll        bool
	OperationCount   int
	WebhookCount     int
	CreatedAt        time.Time
}

type ServiceConsumerRepository interface {
	ListServiceConsumers(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, uuid.UUID) ([]ServiceConsumer, error)
}

func (s *postgresStore) ListServiceConsumers(ctx context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, serviceID uuid.UUID) ([]ServiceConsumer, error) {
	if !scope.All && len(scope.IDs) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT app.app_id, family.display_name,
		       app.version, family.kind, app.status,
		       NULLIF(selection->>'service_version_id', '')::uuid,
		       COALESCE((selection->>'select_all')::boolean, false),
		       jsonb_array_length(COALESCE(selection->'endpoint_ids', '[]'::jsonb))
		         + jsonb_array_length(COALESCE(selection->'operation_names', '[]'::jsonb)),
		       jsonb_array_length(COALESCE(selection->'webhook_ids', '[]'::jsonb))
		         + jsonb_array_length(COALESCE(selection->'webhook_names', '[]'::jsonb)),
		       app.created_at
		FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id AND family.account_id = app.account_id
		CROSS JOIN LATERAL jsonb_array_elements(app.selections) selection
		WHERE app.account_id = $1
		  AND app.status IN ('active', 'deprecated')
		  AND ($2 OR app.app_family_id = ANY($3::uuid[]))
		  AND NULLIF(selection->>'service_id', '')::uuid = $4
		ORDER BY app.created_at DESC, app.app_id`, accountID, scope.All, scope.IDs, serviceID)
	if err != nil {
		return nil, fmt.Errorf("list service consumers: %w", err)
	}
	defer rows.Close()
	consumers := make([]ServiceConsumer, 0)
	for rows.Next() {
		var consumer ServiceConsumer
		if err := rows.Scan(
			&consumer.AppID, &consumer.Name, &consumer.Version, &consumer.Kind, &consumer.Status,
			&consumer.ServiceVersionID, &consumer.SelectAll, &consumer.OperationCount, &consumer.WebhookCount, &consumer.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan service consumer: %w", err)
		}
		consumer.Name = strings.TrimSpace(consumer.Name)
		consumers = append(consumers, consumer)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list service consumers: %w", err)
	}
	return consumers, nil
}

var _ ServiceConsumerRepository = (*postgresStore)(nil)
