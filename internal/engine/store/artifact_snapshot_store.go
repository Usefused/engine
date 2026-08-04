package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrArtifactSnapshotNotFound = errors.New("artifact snapshot not found")

// ArtifactSnapshot is safe, credential-free artifact definition state. Scope,
// bucket and token state intentionally live elsewhere because they cannot be
// reconstructed from Registry after an Engine database reset.
type ArtifactSnapshot struct {
	ArtifactID         uuid.UUID       `json:"artifact_id"`
	AccountID          uuid.UUID       `json:"account_id"`
	Kind               string          `json:"kind"`
	Name               string          `json:"name"`
	Description        string          `json:"description"`
	Version            string          `json:"version"`
	TargetLanguage     string          `json:"target_language"`
	Readme             string          `json:"readme"`
	Selections         json.RawMessage `json:"selections"`
	ScopeSchemaVersion int             `json:"scope_schema_version"`
	SourceHash         string          `json:"source_hash"`
	RegistryCreatedAt  *time.Time      `json:"registry_created_at"`
	FetchedAt          time.Time       `json:"-"`
	RefreshedAt        time.Time       `json:"-"`
	Active             bool            `json:"-"`
}

type ArtifactSnapshotStore interface {
	UpsertArtifactSnapshots(context.Context, []ArtifactSnapshot) error
	DeleteArtifactSnapshot(context.Context, uuid.UUID, uuid.UUID) error
	GetArtifactSnapshot(context.Context, uuid.UUID, uuid.UUID) (*ArtifactSnapshot, error)
	ListArtifactSnapshots(context.Context, uuid.UUID, string, int, int) ([]ArtifactSnapshot, int, error)
}

func (s *postgresStore) DeleteArtifactSnapshot(ctx context.Context, accountID, artifactID uuid.UUID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM fused_artifact_snapshots WHERE account_id = $1 AND artifact_id = $2`, accountID, artifactID)
	return err
}

func (s *postgresStore) UpsertArtifactSnapshots(ctx context.Context, snapshots []ArtifactSnapshot) error {
	if len(snapshots) == 0 {
		return nil
	}
	for _, snapshot := range snapshots {
		if err := validateArtifactSnapshot(snapshot); err != nil {
			return err
		}
	}
	payload, err := json.Marshal(snapshots)
	if err != nil {
		return fmt.Errorf("upsert artifact snapshots: encode batch: %w", err)
	}
	_, err = s.db.Exec(ctx, `
		INSERT INTO fused_artifact_snapshots (
			artifact_id, account_id, kind, name, description, version,
			target_language, readme, selections, scope_schema_version,
			source_hash, registry_created_at
		)
		SELECT artifact_id, account_id, kind, name, description, version,
			target_language, readme, selections, scope_schema_version,
			source_hash, registry_created_at
		FROM jsonb_to_recordset($1::jsonb) AS item(
			artifact_id uuid, account_id uuid, kind text, name text,
			description text, version text, target_language text, readme text,
			selections jsonb, scope_schema_version integer, source_hash text,
			registry_created_at timestamptz
		)
		ON CONFLICT (artifact_id) DO UPDATE SET
			account_id = EXCLUDED.account_id, kind = EXCLUDED.kind,
			name = EXCLUDED.name, description = EXCLUDED.description,
			version = EXCLUDED.version, target_language = EXCLUDED.target_language,
			readme = EXCLUDED.readme, selections = EXCLUDED.selections,
			scope_schema_version = EXCLUDED.scope_schema_version,
			source_hash = EXCLUDED.source_hash,
			registry_created_at = EXCLUDED.registry_created_at,
			refreshed_at = NOW()
	`, payload)
	if err != nil {
		return fmt.Errorf("upsert artifact snapshots: %w", err)
	}
	return nil
}

func (s *postgresStore) GetArtifactSnapshot(ctx context.Context, accountID, artifactID uuid.UUID) (*ArtifactSnapshot, error) {
	row := s.db.QueryRow(ctx, artifactSnapshotSelect+`
		WHERE snapshot.account_id = $1 AND snapshot.artifact_id = $2`, accountID, artifactID)
	snapshot, err := scanArtifactSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrArtifactSnapshotNotFound
	}
	return snapshot, err
}

func (s *postgresStore) ListArtifactSnapshots(ctx context.Context, accountID uuid.UUID, kind string, limit, offset int) ([]ArtifactSnapshot, int, error) {
	kind, valid := normalizeArtifactKind(kind)
	if !valid {
		return nil, 0, ErrInvalidArtifactKind
	}
	var total int
	if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM fused_artifact_snapshots WHERE account_id = $1 AND ($2 = '' OR kind = $2)`, accountID, kind).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(ctx, artifactSnapshotSelect+`
		WHERE snapshot.account_id = $1 AND ($2 = '' OR snapshot.kind = $2)
		ORDER BY snapshot.registry_created_at DESC NULLS LAST, snapshot.artifact_id
		LIMIT $3 OFFSET $4`, accountID, kind, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ArtifactSnapshot, 0, limit)
	for rows.Next() {
		item, scanErr := scanArtifactSnapshot(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		items = append(items, *item)
	}
	return items, total, rows.Err()
}

const artifactSnapshotSelect = `
	SELECT snapshot.artifact_id, snapshot.account_id, snapshot.kind, snapshot.name,
		snapshot.description, snapshot.version, snapshot.target_language,
		snapshot.readme, snapshot.selections, snapshot.scope_schema_version,
		snapshot.source_hash, snapshot.registry_created_at, snapshot.fetched_at,
		snapshot.refreshed_at, scope.artifact_id IS NOT NULL AND scope.deactivated_at IS NULL
	FROM fused_artifact_snapshots snapshot
	LEFT JOIN fused_artifact_scopes scope ON scope.artifact_id = snapshot.artifact_id
`

type artifactSnapshotScanner interface{ Scan(...any) error }

func scanArtifactSnapshot(row artifactSnapshotScanner) (*ArtifactSnapshot, error) {
	var snapshot ArtifactSnapshot
	err := row.Scan(&snapshot.ArtifactID, &snapshot.AccountID, &snapshot.Kind, &snapshot.Name,
		&snapshot.Description, &snapshot.Version, &snapshot.TargetLanguage, &snapshot.Readme,
		&snapshot.Selections, &snapshot.ScopeSchemaVersion, &snapshot.SourceHash,
		&snapshot.RegistryCreatedAt, &snapshot.FetchedAt, &snapshot.RefreshedAt, &snapshot.Active)
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func validateArtifactSnapshot(snapshot ArtifactSnapshot) error {
	if snapshot.ArtifactID == uuid.Nil || snapshot.AccountID == uuid.Nil || snapshot.Name == "" {
		return errors.New("artifact snapshot identity is required")
	}
	if snapshot.Kind != "sdk" && snapshot.Kind != "mcp" {
		return ErrInvalidArtifactKind
	}
	if len(snapshot.Selections) == 0 || !json.Valid(snapshot.Selections) {
		return errors.New("artifact snapshot selections are invalid")
	}
	return nil
}
