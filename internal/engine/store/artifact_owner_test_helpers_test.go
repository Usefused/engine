package store

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func seedArtifactOwnerTeam(t *testing.T, ctx context.Context, db execer) uuid.UUID {
	t.Helper()
	teamID := uuid.New()
	if _, err := db.Exec(ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, $2, $3)`,
		teamID, "Artifact owners", "artifact-owners-"+teamID.String()); err != nil {
		t.Fatalf("seed artifact owner team: %v", err)
	}
	return teamID
}
