package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MCPSkillSnapshotStore reads the Agent Skill snapshot pinned to one immutable MCP version.
type MCPSkillSnapshotStore interface {
	// GetMCPSkillSnapshot returns nil without error for versions planned before skills existed.
	GetMCPSkillSnapshot(ctx context.Context, appID uuid.UUID) (json.RawMessage, error)
}

var _ MCPSkillSnapshotStore = (*postgresStore)(nil)
var _ MCPSkillSnapshotStore = (*cachedStore)(nil)

// GetMCPSkillSnapshot reads mcp_skill from the exact applied plan, like description, so no schema change is needed.
func (s *postgresStore) GetMCPSkillSnapshot(ctx context.Context, appID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `
		SELECT plan.resolved_payload->'mcp_skill'
		FROM fused_apps app
		LEFT JOIN LATERAL (
			SELECT applied.resolved_payload
			FROM fused_config_plans applied
			WHERE applied.config_key = app.config_key AND applied.source_hash = app.source_hash
			  AND applied.status = 'applied'
			ORDER BY applied.applied_at DESC NULLS LAST, applied.created_at DESC
			LIMIT 1
		) plan ON true
		WHERE app.app_id = $1`, appID).Scan(&raw)
	// An unknown app is a lookup failure, never an empty skill for some other version.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAppNotFound
	}
	// Storage failures stay distinct from the legacy no-snapshot case.
	if err != nil {
		return nil, fmt.Errorf("load MCP skill snapshot: %w", err)
	}
	// A JSON null or missing key means the version predates skills.
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	return json.RawMessage(raw), nil
}

// GetMCPSkillSnapshot forwards to the durable delegate; the snapshot is immutable and small, so it is not cached.
func (s *cachedStore) GetMCPSkillSnapshot(ctx context.Context, appID uuid.UUID) (json.RawMessage, error) {
	repository, ok := s.Store.(MCPSkillSnapshotStore)
	// A delegate without the capability must not be mistaken for a version without a skill.
	if !ok {
		return nil, errors.New("MCP skill snapshots are unavailable")
	}
	return repository.GetMCPSkillSnapshot(ctx, appID)
}
