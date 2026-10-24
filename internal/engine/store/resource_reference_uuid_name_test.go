package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestResourceReferenceUUIDShapedNames keeps failed UUID parses on the permission-scoped human-name path.
func TestResourceReferenceUUIDShapedNames(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// Database integration must never guess or modify a developer's ordinary Engine database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)
	accountID, ownerID := uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO fused_teams(id,name,slug) VALUES($1,'Reference regression','reference-regression')`, ownerID)
	recoveryMust(t, err)
	// Cover the deployed 32-character name and a longer parse that fails after a valid hexadecimal prefix.
	for _, name := range []string{"recovery-live-automatic-20261024", "12345678-1234-1234-1234-12345678901z"} {
		// Each case must resolve its own persisted identity without accepting the parser's partial UUID.
		t.Run(name, func(t *testing.T) {
			familyID, appID := uuid.New(), uuid.New()
			_, err := pool.Exec(ctx, `INSERT INTO fused_app_families(app_family_id,account_id,kind,canonical_name,display_name,target_language,owner_team_id) VALUES($1,$2,'unified_app',$3,$3,'typescript',$4)`, familyID, accountID, name, ownerID)
			recoveryMust(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO fused_apps(app_id,app_family_id,account_id,version,config_key,source_hash,status,selections) VALUES($1,$2,$3,'1.0.0',$4,'reference-regression','active','[]')`, appID, familyID, accountID, "unified_app:"+name+":1.0.0")
			recoveryMust(t, err)
			familyQuery := ResourceReferenceQuery{Kind: ReferenceAppFamily, Value: name, AppKind: AppKindUnifiedApp, AllowedIDs: []uuid.UUID{familyID}}
			assertReferenceID(t, ctx, repository, familyQuery, familyID)
			versionQuery := ResourceReferenceQuery{Kind: ReferenceApp, Value: name, AppKind: AppKindUnifiedApp, AppVersion: "1.0.0", AllowedIDs: []uuid.UUID{familyID}}
			assertReferenceID(t, ctx, repository, versionQuery, appID)
			familyQuery.Value = familyID.String()
			assertReferenceID(t, ctx, repository, familyQuery, familyID)
			familyQuery.Value, familyQuery.AllowedIDs = name, []uuid.UUID{uuid.New()}
			_, err = repository.ResolveResourceReference(ctx, familyQuery)
			// Falling back to a human name must not widen the caller's allowed family set.
			if !errors.Is(err, ErrResourceReferenceNotFound) {
				t.Fatalf("unauthorized name lookup returned %v", err)
			}
		})
	}
}
