package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

type ArtifactServiceSummary struct {
	ServiceID     uuid.UUID
	ServiceSlug   string
	ServiceName   string
	Version       string
	SelectAll     bool
	EndpointCount int
	WebhookCount  int
}

type ArtifactServiceRepository interface {
	ListArtifactServiceSummaries(context.Context, uuid.UUID) ([]ArtifactServiceSummary, error)
	ListArtifactSnapshotServiceSummaries(context.Context, uuid.UUID, uuid.UUID) ([]ArtifactServiceSummary, error)
}

func (s *postgresStore) ListArtifactServiceSummaries(ctx context.Context, artifactID uuid.UUID) ([]ArtifactServiceSummary, error) {
	// Expand the persisted selection JSON and join service/version labels in
	// one query. Resolving each selected service separately would make this
	// user-facing command scale as N+1 with artifact size.
	rows, err := s.db.Query(ctx, `
		SELECT service.service_id, COALESCE(service.service_slug, ''), COALESCE(service.service_name, ''),
		       COALESCE(version.version, ''), COALESCE((selection->>'select_all')::boolean, false),
		       jsonb_array_length(COALESCE(selection->'endpoint_ids', '[]'::jsonb)),
		       jsonb_array_length(COALESCE(selection->'webhook_ids', '[]'::jsonb))
		FROM fused_artifact_scopes artifact
		CROSS JOIN LATERAL jsonb_array_elements(artifact.selections) selection
		JOIN fused_workspace_services service ON service.service_id = (selection->>'service_id')::uuid
		LEFT JOIN fused_workspace_service_versions version
		  ON version.service_id = service.service_id
		 AND version.service_version_id = NULLIF(selection->>'service_version_id', '')::uuid
		WHERE artifact.artifact_id = $1
		ORDER BY COALESCE(service.service_slug, service.service_name), service.service_id`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list artifact services: %w", err)
	}
	defer rows.Close()
	return scanArtifactServiceSummaries(rows)
}

func (s *postgresStore) ListArtifactSnapshotServiceSummaries(ctx context.Context, accountID, artifactID uuid.UUID) ([]ArtifactServiceSummary, error) {
	// Snapshot definitions remain readable when runtime scope tables have been
	// cleared. Resolve every selected service in SQL to avoid per-service reads.
	rows, err := s.db.Query(ctx, `
		SELECT service.service_id, COALESCE(service.service_slug, ''), COALESCE(service.service_name, ''),
		       COALESCE(version.version, ''), COALESCE((selection->>'select_all')::boolean, false),
		       jsonb_array_length(COALESCE(selection->'endpoint_ids', '[]'::jsonb)),
		       jsonb_array_length(COALESCE(selection->'webhook_ids', '[]'::jsonb))
		FROM fused_artifact_snapshots snapshot
		CROSS JOIN LATERAL jsonb_array_elements(snapshot.selections) selection
		JOIN fused_workspace_services service ON service.service_id = (selection->>'service_id')::uuid
		LEFT JOIN fused_workspace_service_versions version
		  ON version.service_id = service.service_id
		 AND version.service_version_id = NULLIF(selection->>'service_version_id', '')::uuid
		WHERE snapshot.account_id = $1 AND snapshot.artifact_id = $2
		ORDER BY COALESCE(service.service_slug, service.service_name), service.service_id`, accountID, artifactID)
	if err != nil {
		return nil, fmt.Errorf("list artifact snapshot services: %w", err)
	}
	defer rows.Close()
	return scanArtifactServiceSummaries(rows)
}

type artifactServiceRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

func scanArtifactServiceSummaries(rows artifactServiceRows) ([]ArtifactServiceSummary, error) {
	services := make([]ArtifactServiceSummary, 0)
	for rows.Next() {
		var service ArtifactServiceSummary
		if err := rows.Scan(&service.ServiceID, &service.ServiceSlug, &service.ServiceName, &service.Version, &service.SelectAll, &service.EndpointCount, &service.WebhookCount); err != nil {
			return nil, fmt.Errorf("scan artifact service: %w", err)
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artifact services: %w", err)
	}
	return services, nil
}

var _ ArtifactServiceRepository = (*postgresStore)(nil)
