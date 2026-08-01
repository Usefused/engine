package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

func (s *postgresStore) QueryAuditEvents(ctx context.Context, input AuditQuery) (AuditPage, error) {
	if err := validateAuditQuery(input.RequesterSubjectID, input.Actions, input.Outcomes, input.From, input.To, input.Limit, 200); err != nil {
		return AuditPage{}, err
	}
	if input.After != nil && (input.After.ID == uuid.Nil || input.After.OccurredAt.IsZero()) {
		return AuditPage{}, ErrInvalidAuditQuery
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.audit.query")
	defer span.End()
	afterAt, afterID := auditAfterValues(input.After)
	rows, err := s.db.Query(ctx, auditPageSQL, input.RequesterSubjectID, input.ActorSubjectID, input.Actions,
		auditOutcomeStrings(input.Outcomes), input.From, input.To, afterAt, afterID, input.Limit)
	if err != nil {
		return AuditPage{}, fmt.Errorf("query audit events: %w", err)
	}
	defer rows.Close()
	return collectAuditPage(rows, input.Limit)
}

func collectAuditPage(rows pgx.Rows, limit int) (AuditPage, error) {
	page := AuditPage{Items: make([]AuditRecord, 0, limit)}
	var remaining int
	for rows.Next() {
		item, rowRemaining, total, err := scanAuditRecord(rows)
		if err != nil {
			return AuditPage{}, err
		}
		page.Total, remaining = total, rowRemaining
		if item != nil {
			page.Items = append(page.Items, *item)
		}
	}
	if err := rows.Err(); err != nil {
		return AuditPage{}, fmt.Errorf("iterate audit events: %w", err)
	}
	if remaining > len(page.Items) && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &AuditCursor{OccurredAt: last.OccurredAt, ID: last.ID}
	}
	return page, nil
}

func (s *postgresStore) ExportAuditEvents(ctx context.Context, input AuditExportQuery) ([]AuditExportRow, error) {
	if err := validateAuditQuery(input.RequesterSubjectID, input.Actions, input.Outcomes, input.From, input.To, input.Limit, 10000); err != nil {
		return nil, err
	}
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.audit.export")
	defer span.End()
	span.SetAttributes(attribute.Int("limit", input.Limit))
	rows, err := s.db.Query(ctx, auditExportSQL, input.RequesterSubjectID, input.ActorSubjectID, input.Actions,
		auditOutcomeStrings(input.Outcomes), input.From, input.To, input.Limit)
	if err != nil {
		return nil, fmt.Errorf("export audit events: %w", err)
	}
	defer rows.Close()
	items := make([]AuditExportRow, 0, input.Limit)
	for rows.Next() {
		var item AuditExportRow
		var permission, resourceType *string
		var resourceID *uuid.UUID
		var missingRequirements json.RawMessage
		if err := rows.Scan(&item.ID, &item.OccurredAt, &item.ActorSubjectID, &item.ActorCredentialID, &item.Action,
			&permission, &resourceType, &resourceID, &item.RequestID, &item.TraceID, &item.Method, &item.Path,
			&item.Outcome, &item.StatusCode, &item.ReasonCode, &missingRequirements); err != nil {
			return nil, fmt.Errorf("scan audit export: %w", err)
		}
		item.MissingRequirements, err = accesscontrol.UnmarshalRequiredPermissions(missingRequirements)
		if err != nil {
			return nil, fmt.Errorf("decode missing audit requirements: %w", err)
		}
		setAuditPermissionResource(permission, resourceType, resourceID, &item.Permission, &item.Resource)
		items = append(items, item)
	}
	return items, rows.Err()
}

func auditAfterValues(cursor *AuditCursor) (*time.Time, *uuid.UUID) {
	if cursor == nil {
		return nil, nil
	}
	return &cursor.OccurredAt, &cursor.ID
}

func auditOutcomeStrings(outcomes []accesscontrol.AuditOutcome) []string {
	values := make([]string, len(outcomes))
	for index, outcome := range outcomes {
		values[index] = string(outcome)
	}
	return values
}

func scanAuditRecord(row pgx.Row) (*AuditRecord, int, int, error) {
	var item AuditRecord
	var id *uuid.UUID
	var occurredAt *time.Time
	var permission, resourceType *string
	var resourceID *uuid.UUID
	var metadata, missingRequirements json.RawMessage
	var remaining, total int
	err := row.Scan(&id, &occurredAt, &item.ActorSubjectID, &item.ActorCredentialID, &item.Action, &permission, &resourceType,
		&resourceID, &item.RequestID, &item.TraceID, &item.Method, &item.Path, &item.Outcome, &item.StatusCode,
		&item.ReasonCode, &missingRequirements, &metadata, &remaining, &total)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("scan audit event: %w", err)
	}
	if id == nil {
		return nil, remaining, total, nil
	}
	item.ID, item.OccurredAt = *id, *occurredAt
	setAuditPermissionResource(permission, resourceType, resourceID, &item.Permission, &item.Resource)
	item.MissingRequirements, err = accesscontrol.UnmarshalRequiredPermissions(missingRequirements)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode missing audit requirements: %w", err)
	}
	item.Metadata, err = sanitizeStoredAuditMetadata(metadata)
	if err != nil {
		return nil, 0, 0, err
	}
	return &item, remaining, total, nil
}

func setAuditPermissionResource(permission, resourceType *string, resourceID *uuid.UUID, targetPermission *accesscontrol.Permission, targetResource *accesscontrol.ResourceRef) {
	if permission != nil {
		*targetPermission = accesscontrol.Permission(*permission)
	}
	if resourceType != nil && resourceID != nil {
		*targetResource = accesscontrol.ResourceRef{Type: accesscontrol.ResourceType(*resourceType), ID: *resourceID}
	}
}

const auditAuthorizationSQL = `
WITH requester_principals(subject_type, subject_id) AS (
	SELECT 'subject'::text, subject.id FROM fused_subjects subject WHERE subject.id = $1 AND subject.status = 'active'
	UNION ALL
	SELECT 'team'::text, membership.team_id FROM fused_team_memberships membership
	JOIN fused_subjects subject ON subject.id = membership.member_subject_id AND subject.status = 'active'
	JOIN fused_teams team ON team.id = membership.team_id AND team.status = 'active'
	WHERE membership.member_subject_id = $1
), authorized AS (
	SELECT EXISTS (
		SELECT 1 FROM requester_principals principal
		JOIN fused_role_bindings binding ON binding.subject_type = principal.subject_type AND binding.subject_id = principal.subject_id
		JOIN fused_roles role ON role.id = binding.role_id AND role.scope_type = 'workspace'
		JOIN fused_role_permissions permission ON permission.role_id = role.id AND permission.permission = 'audit.read'
		JOIN fused_workspaces workspace ON workspace.singleton_key = 1 AND workspace.id = binding.resource_id
		WHERE binding.resource_type = 'workspace'
	) AS allowed
), base AS (
	SELECT event.* FROM fused_audit_events event, authorized
	WHERE authorized.allowed
		AND ($2::uuid IS NULL OR event.actor_subject_id = $2)
		AND (COALESCE(cardinality($3::text[]), 0) = 0 OR event.action = ANY($3::text[]))
		AND (COALESCE(cardinality($4::text[]), 0) = 0 OR event.outcome = ANY($4::text[]))
		AND ($5::timestamptz IS NULL OR event.occurred_at >= $5)
		AND ($6::timestamptz IS NULL OR event.occurred_at <= $6)
) `

const auditPageSQL = auditAuthorizationSQL + `,
eligible AS (
	SELECT * FROM base WHERE $7::timestamptz IS NULL OR (occurred_at, id) < ($7, $8::uuid)
), page AS (
	SELECT * FROM eligible ORDER BY occurred_at DESC, id DESC LIMIT $9
), summary AS (
	SELECT (SELECT COUNT(*) FROM eligible)::int AS remaining, (SELECT COUNT(*) FROM base)::int AS total
)
SELECT page.id, page.occurred_at, page.actor_subject_id, page.actor_credential_id, COALESCE(page.action, ''),
	page.permission, page.resource_type, page.resource_id, COALESCE(page.request_id, ''), COALESCE(page.trace_id, ''), COALESCE(page.method, ''),
	COALESCE(page.path, ''), COALESCE(page.outcome, ''), COALESCE(page.status_code, 0), COALESCE(page.reason_code, ''),
	COALESCE(page.missing_requirements, '[]'::jsonb), COALESCE(page.metadata, '{}'::jsonb), summary.remaining, summary.total
FROM summary LEFT JOIN page ON true ORDER BY page.occurred_at DESC, page.id DESC`

const auditExportSQL = auditAuthorizationSQL + `
SELECT id, occurred_at, actor_subject_id, actor_credential_id, action, permission, resource_type,
	resource_id, request_id, trace_id, method, path, outcome, status_code, reason_code, missing_requirements
FROM base ORDER BY occurred_at DESC, id DESC LIMIT $7`

var _ AuditRepository = (*postgresStore)(nil)
