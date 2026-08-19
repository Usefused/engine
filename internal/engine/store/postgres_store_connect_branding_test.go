package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type connectBrandingPostgresFixture struct {
	ctx         context.Context
	pool        *pgxpool.Pool
	repository  Store
	workspaceID uuid.UUID
}

// TestPostgresConnectBrandingDefaultsAndReplacement verifies the migrated
// singleton row supplies defaults and persists one atomic full replacement.
func TestPostgresConnectBrandingDefaultsAndReplacement(t *testing.T) {
	fixture := newConnectBrandingPostgresFixture(t)
	assertConnectBrandingDefaultsAndReplacement(t, fixture)
}

// newConnectBrandingPostgresFixture creates one disposable singleton fixture
// and registers exact-row cleanup against the dedicated integration database.
func newConnectBrandingPostgresFixture(t *testing.T) connectBrandingPostgresFixture {
	t.Helper()
	databaseURL := os.Getenv("CONNECT_BRANDING_TEST_DATABASE_URL")
	// Missing isolation configuration skips instead of borrowing DATABASE_URL.
	if databaseURL == "" {
		// A dedicated variable prevents this singleton test from touching a
		// developer Engine database that may hold live OAuth connections.
		t.Skip("CONNECT_BRANDING_TEST_DATABASE_URL is required for isolated PostgreSQL coverage")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Initialization errors identify an invalid disposable database setup.
	if err != nil {
		t.Fatalf("initialize Engine PostgreSQL: %v", err)
	}
	// Pool cleanup is registered first so the later exact-fixture cleanup runs
	// while its database connection is still available.
	t.Cleanup(pool.Close)
	repository := NewPostgresStore(pool)
	accountID := uuid.New()
	workspaceID, err := repository.BootstrapWorkspace(ctx, accountID, "Branding test")
	// The singleton must be available before defaults or replacements can be tested.
	if err != nil {
		t.Fatalf("bootstrap workspace: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// Cleanup targets only the exact disposable fixture created in the
		// dedicated test database and never broad-deletes workspace data.
		if _, cleanupErr := pool.Exec(cleanupCtx, "DELETE FROM fused_workspaces WHERE id = $1 AND account_id = $2", workspaceID, accountID); cleanupErr != nil {
			t.Errorf("remove connect-branding fixture: %v", cleanupErr)
		}
	})
	return connectBrandingPostgresFixture{ctx: ctx, pool: pool, repository: repository, workspaceID: workspaceID}
}

// assertConnectBrandingDefaultsAndReplacement checks the public projection and
// internal customization marker after one atomic full replacement.
func assertConnectBrandingDefaultsAndReplacement(t *testing.T, fixture connectBrandingPostgresFixture) {
	t.Helper()
	defaults, err := fixture.repository.GetConnectBranding(fixture.ctx)
	// Fresh workspaces must expose the same default as the compiled fallback.
	if err != nil || defaults != DefaultConnectBranding() {
		t.Fatalf("default connect branding = %#v, %v", defaults, err)
	}
	want := ConnectBranding{
		DisplayName: "Acme", LogoURL: "https://assets.example.com/logo.png", PrimaryColor: "#123456",
		SupportURL: "https://help.example.com", PrivacyURL: "https://legal.example.com/privacy",
	}
	saved, err := fixture.repository.UpsertConnectBranding(fixture.ctx, want)
	// The update response must reflect the complete normalized replacement.
	if err != nil || saved != want {
		t.Fatalf("saved connect branding = %#v, %v", saved, err)
	}
	loaded, err := fixture.repository.GetConnectBranding(fixture.ctx)
	// A separate read proves the values were persisted rather than echoed.
	if err != nil || loaded != want {
		t.Fatalf("loaded connect branding = %#v, %v", loaded, err)
	}
	var customized bool
	// The internal marker prevents a later default migration from replacing the choice.
	if err := fixture.pool.QueryRow(fixture.ctx, `SELECT connect_primary_color_customized FROM fused_workspaces WHERE id = $1`, fixture.workspaceID).Scan(&customized); err != nil || !customized {
		t.Fatalf("explicit brand colour was not protected: customized=%v err=%v", customized, err)
	}
}

// TestUpsertConnectBrandingMarksOnlyARealColourChange protects future default
// migrations without treating unrelated logo or copy changes as color choices.
func TestUpsertConnectBrandingMarksOnlyARealColourChange(t *testing.T) {
	// Both SQL fragments are required to preserve prior evidence and detect a
	// new explicit colour change in the same atomic update.
	for _, expected := range []string{
		"connect_primary_color_customized OR connect_primary_color IS DISTINCT FROM $3",
		"connect_primary_color = $3",
	} {
		// A missing fragment would let a later migration overwrite a user choice.
		if !strings.Contains(upsertConnectBrandingSQL, expected) {
			t.Fatalf("connect branding upsert missing %q: %s", expected, upsertConnectBrandingSQL)
		}
	}
}
