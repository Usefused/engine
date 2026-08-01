package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

func TestPostgresStore_GetMCPScopeByName(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Postgres store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	s := NewPostgresStore(pool)
	accountID := uuid.New()
	ownerTeamID := seedArtifactOwnerTeam(t, ctx, pool)

	// Create version 0.1.0
	artifactID1 := uuid.New()
	err = s.SaveArtifactScope(ctx, ArtifactScope{
		AccountID:          accountID,
		ArtifactID:         artifactID1,
		OwnerTeamID:        ownerTeamID,
		ScopeSchemaVersion: 1,
		Selections:         []byte("{}"),
		Kind:               "mcp",
		Name:               "test-mcp",
		Version:            "0.1.0",
		ConfigKey:          "mcp:test-mcp:0.1.0",
	})
	if err != nil {
		t.Fatalf("SaveArtifactScope 0.1.0 failed: %v", err)
	}

	// Create version 0.2.0
	artifactID2 := uuid.New()
	err = s.SaveArtifactScope(ctx, ArtifactScope{
		AccountID:          accountID,
		ArtifactID:         artifactID2,
		OwnerTeamID:        ownerTeamID,
		ScopeSchemaVersion: 1,
		Selections:         []byte("{}"),
		Kind:               "mcp",
		Name:               "test-mcp",
		Version:            "0.2.0",
		ConfigKey:          "mcp:test-mcp:0.2.0",
	})
	if err != nil {
		t.Fatalf("SaveArtifactScope 0.2.0 failed: %v", err)
	}

	t.Run("GetMCPScopeByName with version returns exact match", func(t *testing.T) {
		scope, err := s.GetMCPScopeByName(ctx, accountID, "test-mcp", "0.1.0")
		if err != nil {
			t.Fatalf("GetMCPScopeByName failed: %v", err)
		}
		if scope.ArtifactID != artifactID1 {
			t.Errorf("expected ArtifactID %s, got %s", artifactID1, scope.ArtifactID)
		}
	})

	t.Run("GetMCPScopeByName without version returns most recent", func(t *testing.T) {
		scope, err := s.GetMCPScopeByName(ctx, accountID, "test-mcp", "")
		if err != nil {
			t.Fatalf("GetMCPScopeByName failed: %v", err)
		}
		// 0.2.0 was created after 0.1.0
		if scope.ArtifactID != artifactID2 {
			t.Errorf("expected ArtifactID %s (latest), got %s", artifactID2, scope.ArtifactID)
		}
	})

	t.Run("GetMCPScopeByName returns ErrArtifactScopeNotFound when missing", func(t *testing.T) {
		_, err := s.GetMCPScopeByName(ctx, accountID, "nonexistent-mcp", "")
		if !errors.Is(err, ErrArtifactScopeNotFound) {
			t.Errorf("expected ErrArtifactScopeNotFound, got %v", err)
		}
	})

	t.Run("GetMCPScopeByName respects account isolation", func(t *testing.T) {
		_, err := s.GetMCPScopeByName(ctx, uuid.New(), "test-mcp", "")
		if !errors.Is(err, ErrArtifactScopeNotFound) {
			t.Errorf("expected ErrArtifactScopeNotFound for different account, got %v", err)
		}
	})
}
