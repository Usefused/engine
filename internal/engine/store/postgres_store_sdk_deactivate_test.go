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

// TestPostgresStore_DeactivateReactivateSDK covers the store-layer half of
// the activate/deactivate lifecycle endpoints (api.DeactivateSDKHandler,
// api.ActivateSDKHandler): DeactivateSDK/ReactivateSDK toggle
// fused_artifact_scopes.deactivated_at, scoped by account_id so one account can
// never flip another account's SDK, and both report ErrArtifactScopeNotFound
// (not a generic error) when there's nothing to act on -- the HTTP handlers
// depend on that specific error to return 404 instead of 500.
func TestPostgresStore_DeactivateReactivateSDK(t *testing.T) {
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
	workspaceID := uuid.New()
	artifactID := uuid.New()
	ownerTeamID := seedArtifactOwnerTeam(t, ctx, pool)

	if _, err := pool.Exec(ctx, "DELETE FROM fused_workspaces"); err != nil {
		t.Fatalf("clean workspace: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO fused_workspaces (id, account_id, name, slug) VALUES ($1, $2, $3, $4)", workspaceID, accountID, "Test WS", "test-ws"); err != nil {
		t.Fatalf("setup workspace failed: %v", err)
	}
	if err := s.SaveArtifactScope(ctx, ArtifactScope{
		AccountID:          accountID,
		ArtifactID:         artifactID,
		OwnerTeamID:        ownerTeamID,
		Selections:         []byte("[]"),
		ScopeSchemaVersion: 1,
	}); err != nil {
		t.Fatalf("SaveArtifactScope failed: %v", err)
	}

	t.Run("DeactivateSDK against an unknown sdk returns ErrArtifactScopeNotFound", func(t *testing.T) {
		if err := s.DeactivateSDK(ctx, accountID, uuid.New()); !errors.Is(err, ErrArtifactScopeNotFound) {
			t.Fatalf("expected ErrArtifactScopeNotFound, got %v", err)
		}
	})

	t.Run("DeactivateSDK from another account does not affect the scope", func(t *testing.T) {
		if err := s.DeactivateSDK(ctx, uuid.New(), artifactID); !errors.Is(err, ErrArtifactScopeNotFound) {
			t.Fatalf("expected ErrArtifactScopeNotFound for cross-account deactivate, got %v", err)
		}
		scope, err := s.GetArtifactScope(ctx, artifactID)
		if err != nil {
			t.Fatalf("GetArtifactScope failed: %v", err)
		}
		if scope.DeactivatedAt != nil {
			t.Fatal("a cross-account deactivate attempt must not have taken effect")
		}
	})

	t.Run("DeactivateSDK sets deactivated_at, ReactivateSDK clears it", func(t *testing.T) {
		if err := s.DeactivateSDK(ctx, accountID, artifactID); err != nil {
			t.Fatalf("DeactivateSDK failed: %v", err)
		}
		scope, err := s.GetArtifactScope(ctx, artifactID)
		if err != nil {
			t.Fatalf("GetArtifactScope failed: %v", err)
		}
		if scope.DeactivatedAt == nil {
			t.Fatal("expected DeactivatedAt to be set after DeactivateSDK")
		}
		if scope.ScopeSchemaVersion != 1 || string(scope.Selections) != "[]" {
			t.Fatalf("DeactivateSDK must not touch selections/schema version, got %#v", scope)
		}

		if err := s.ReactivateSDK(ctx, accountID, artifactID); err != nil {
			t.Fatalf("ReactivateSDK failed: %v", err)
		}
		scope, err = s.GetArtifactScope(ctx, artifactID)
		if err != nil {
			t.Fatalf("GetArtifactScope failed: %v", err)
		}
		if scope.DeactivatedAt != nil {
			t.Fatal("expected DeactivatedAt to be cleared after ReactivateSDK")
		}
	})

	t.Run("ReactivateSDK is idempotent against an already-active scope", func(t *testing.T) {
		if err := s.ReactivateSDK(ctx, accountID, artifactID); err != nil {
			t.Fatalf("expected a second reactivate to succeed as a no-op, got %v", err)
		}
	})
}
