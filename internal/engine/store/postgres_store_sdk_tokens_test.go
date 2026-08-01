package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
)

func TestPostgresStore_SDKTokens(t *testing.T) {
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
	artifactID := uuid.New()
	ownerTeamID := seedArtifactOwnerTeam(t, ctx, pool)

	err = s.SaveArtifactScope(ctx, ArtifactScope{
		AccountID:          accountID,
		ArtifactID:         artifactID,
		OwnerTeamID:        ownerTeamID,
		Selections:         []byte("[]"),
		ScopeSchemaVersion: 1,
	})
	if err != nil {
		t.Fatalf("SaveArtifactScope failed: %v", err)
	}

	// Test CreateSDKToken
	tokenHash := "test-hash-" + uuid.NewString()
	tokenName := "default"
	tok, err := s.CreateSDKToken(ctx, artifactID, tokenHash, tokenName)
	if err != nil {
		t.Fatalf("CreateSDKToken failed: %v", err)
	}
	if tok.TokenHash != tokenHash || tok.Name != tokenName {
		t.Errorf("unexpected token data: %+v", tok)
	}

	// Test GetArtifactByToken
	fetched, err := s.GetArtifactByToken(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetArtifactByToken failed: %v", err)
	}
	if fetched.ArtifactID != artifactID {
		t.Errorf("GetArtifactByToken returned wrong ArtifactID: %v", fetched.ArtifactID)
	}

	// Test ListSDKTokens
	tokens, err := s.ListSDKTokens(ctx, artifactID)
	if err != nil {
		t.Fatalf("ListSDKTokens failed: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("ListSDKTokens expected 1 token, got %d", len(tokens))
	} else if tokens[0].LastUsedAt == nil {
		t.Errorf("expected LastUsedAt to be updated by GetArtifactByToken")
	}

	// Test RevokeSDKToken
	if err := s.RevokeSDKToken(ctx, artifactID, tokenName); err != nil {
		t.Fatalf("RevokeSDKToken failed: %v", err)
	}

	tokens, err = s.ListSDKTokens(ctx, artifactID)
	if err != nil || len(tokens) != 0 {
		t.Errorf("expected token to be deleted, got %d", len(tokens))
	}
}
