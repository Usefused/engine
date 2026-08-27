package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestAppCredentialSourceMigrationPreservesLiveReferenceState upgrades a populated real PostgreSQL v15 schema.
func TestAppCredentialSourceMigrationPreservesLiveReferenceState(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	// PostgreSQL-backed migration coverage remains opt-in for explicit local or manual test runs.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedEngineMigrationPool(t, ctx, databaseURL)
	applyAppCredentialSourceV15FixtureSchema(t, ctx, pool)
	fixture := seedAppCredentialSourceV15Rows(t, ctx, pool)
	if err := initEngineSchema(ctx, pool); err != nil {
		t.Fatalf("upgrade populated v15 credential references: %v", err)
	}
	assertAppCredentialSourceUpgrade(t, ctx, pool, fixture)
}

type appCredentialSourceUpgradeFixture struct {
	appID                 uuid.UUID
	inputSessionID        uuid.UUID
	connectID             uuid.UUID
	connectionID          uuid.UUID
	blankConnectID        uuid.UUID
	blankConnectionID     uuid.UUID
	ambiguousConnectID    uuid.UUID
	ambiguousConnectionID uuid.UUID
	targetID              uuid.UUID
	requiredTargetID      uuid.UUID
	ambiguousTargetID     uuid.UUID
	sourceID              uuid.UUID
	targetVersionID       uuid.UUID
	requiredVersionID     uuid.UUID
	ambiguousVersionID    uuid.UUID
}

// applyAppCredentialSourceV15FixtureSchema reconstructs the shipped v15 columns and immutable ledger state.
func applyAppCredentialSourceV15FixtureSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range engineSchemaQueries() {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("create canonical credential-source fixture schema: %v", err)
		}
	}
	seededAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, migration := range engineMigrations()[:15] {
		if _, err := pool.Exec(ctx, `INSERT INTO fused_engine_schema_migrations (version, name, applied_at) VALUES ($1, $2, $3)`, migration.Version, migration.Name, seededAt); err != nil {
			t.Fatalf("seed migration %d identity: %v", migration.Version, err)
		}
	}
	queries := []string{
		`ALTER TABLE fused_connect_input_sessions DROP COLUMN credential_source_service_id, DROP COLUMN credential_source_auth_type, DROP COLUMN credential_source_auth_name`,
		`ALTER TABLE fused_connect_sessions DROP COLUMN credential_source_service_id, DROP COLUMN credential_source_auth_type, DROP COLUMN credential_source_auth_name, DROP COLUMN redirect_uri`,
		`ALTER TABLE fused_auth_connections DROP COLUMN credential_source_service_id, DROP COLUMN credential_source_auth_type, DROP COLUMN credential_source_auth_name`,
	}
	for _, query := range queries {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatalf("restore v15 credential-source shape: %v", err)
		}
	}
}

// seedAppCredentialSourceV15Rows creates referenced app, input-session, callback-session, and grant rows.
func seedAppCredentialSourceV15Rows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) appCredentialSourceUpgradeFixture {
	t.Helper()
	fixture := appCredentialSourceUpgradeFixture{
		appID: uuid.New(), inputSessionID: uuid.New(), connectID: uuid.New(), connectionID: uuid.New(),
		blankConnectID: uuid.New(), blankConnectionID: uuid.New(),
		ambiguousConnectID: uuid.New(), ambiguousConnectionID: uuid.New(),
		targetID: uuid.New(), requiredTargetID: uuid.New(), ambiguousTargetID: uuid.New(), sourceID: uuid.New(),
		targetVersionID: uuid.New(), requiredVersionID: uuid.New(), ambiguousVersionID: uuid.New(),
	}
	bucketID, accountID, ownerID, familyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	execAppCredentialSourceFixture(t, ctx, pool, "seed migration owner",
		`INSERT INTO fused_subjects (id, kind, display_name) VALUES ($1, 'user', 'Migration owner')`, ownerID)
	execAppCredentialSourceFixture(t, ctx, pool, "seed migration bucket",
		`INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`, bucketID, "auth-ref-"+bucketID.String())
	// Distinct target services let one app exercise named, unique-required, and ambiguous-required migration paths.
	execAppCredentialSourceFixture(t, ctx, pool, "seed migration services", `INSERT INTO fused_workspace_services (service_id, service_slug, service_name) VALUES
		($1, 'google-sheets', 'Google Sheets'), ($2, 'google-drive', 'Google Drive'),
		($3, 'google-calendar', 'Google Calendar'), ($4, 'gmail', 'Gmail')`,
		fixture.targetID, fixture.requiredTargetID, fixture.ambiguousTargetID, fixture.sourceID)
	// Every target keeps an exact immutable version so unrelated session migration assertions remain realistic.
	execAppCredentialSourceFixture(t, ctx, pool, "seed migration service versions", `INSERT INTO fused_workspace_service_versions (service_id, service_version_id, version, status) VALUES
		($1, $2, 'v4', 'public'), ($3, $4, 'v3', 'public'), ($5, $6, 'v3', 'public'), ($7, $8, 'v1', 'public')`,
		fixture.targetID, fixture.targetVersionID, fixture.requiredTargetID, fixture.requiredVersionID,
		fixture.ambiguousTargetID, fixture.ambiguousVersionID, fixture.sourceID, uuid.New())
	// Pinned metadata distinguishes one recoverable blank session from an ambiguous two-scheme session.
	execAppCredentialSourceFixture(t, ctx, pool, "seed pinned migration auth contracts", `INSERT INTO fused_service_contract_snapshots
		(service_id, service_version_id, version, contract_version, required_capabilities, contract_hash, service_metadata)
		VALUES
		($1, $2, 'v4', 1, '{}', $3, '{"auth_configs":[{"type":"oauth2","name":"oauth2"}]}'),
		($4, $5, 'v3', 1, '{}', $6, '{"auth_configs":[{"type":"oauth2","name":"oauth2"},{"type":"oauth2","name":"secondaryOAuth"}]}')`,
		fixture.targetID, fixture.targetVersionID, "sha256:"+strings.Repeat("1", 64),
		fixture.ambiguousTargetID, fixture.ambiguousVersionID, "sha256:"+strings.Repeat("2", 64))
	// The ambiguous target deliberately owns two required-family edges so v17 cannot choose by row order.
	execAppCredentialSourceFixture(t, ctx, pool, "seed workspace auth reference", `INSERT INTO fused_workspace_auth_references
		(bucket_id, target_service_id, target_auth_type, target_auth_name, source_service_id, source_auth_type, source_auth_name)
		VALUES ($1, $2, 'oauth', 'oauth2', $5, 'oauth', 'oauth2'),
		       ($1, $3, 'oauth', 'oauth2', $5, 'oauth', 'oauth2'),
		       ($1, $4, 'oauth', 'oauth2', $5, 'oauth', 'oauth2'),
		       ($1, $4, 'oauth', 'secondaryOAuth', $5, 'oauth', 'secondaryOAuth')`,
		bucketID, fixture.targetID, fixture.requiredTargetID, fixture.ambiguousTargetID, fixture.sourceID)
	execAppCredentialSourceFixture(t, ctx, pool, "seed app family", `INSERT INTO fused_app_families
		(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_subject_id)
		VALUES ($1, $2, 'sdk', 'sheets-reader', 'Sheets reader', 'typescript', $3)`, familyID, accountID, ownerID)
	execAppCredentialSourceFixture(t, ctx, pool, "seed app bucket",
		`INSERT INTO fused_app_family_buckets (app_family_id, bucket_id) VALUES ($1, $2)`, familyID, bucketID)
	execAppCredentialSourceFixture(t, ctx, pool, "seed app config state", `INSERT INTO fused_config_states
		(config_key, config_type, owner_subject_id, source_hash, desired_state)
		VALUES ('sdk:sheets-reader:1.0.0', 'sdk', $1, 'sha256:fixture', '{"services":{"google-sheets":{"auth":{"ref":"${bucket.auth.gmail.oauth2}"}}}}')`, ownerID)
	selection := `[{"schema_version":3,"service_id":"` + fixture.targetID.String() + `","service_version_id":"` + fixture.targetVersionID.String() + `","auth_type":"oauth","auth_name":"oauth2"},` +
		`{"schema_version":3,"service_id":"` + fixture.requiredTargetID.String() + `","service_version_id":"` + fixture.requiredVersionID.String() + `","auth_type":"","auth_name":"","required_auth":[{"auth_type":"oauth","auth_name":"oauth2"}]},` +
		`{"schema_version":3,"service_id":"` + fixture.ambiguousTargetID.String() + `","service_version_id":"` + fixture.ambiguousVersionID.String() + `","auth_type":"","auth_name":"","required_auth":[{"auth_type":"oauth","auth_name":"oauth2"},{"auth_type":"oauth","auth_name":"secondaryOAuth"}]}]`
	// One immutable app row carries all selection cardinality cases through the same JSON aggregation.
	execAppCredentialSourceFixture(t, ctx, pool, "seed referenced app", `INSERT INTO fused_apps
		(app_id, app_family_id, account_id, version, config_key, scope_schema_version, selections, status)
		VALUES ($1, $2, $3, '1.0.0', 'sdk:sheets-reader:1.0.0', 3, $4::jsonb, 'active')`, fixture.appID, familyID, accountID, selection)
	execAppCredentialSourceFixture(t, ctx, pool, "seed referenced input session", `INSERT INTO fused_connect_input_sessions
		(id, bucket_id, service_id, auth_type, auth_name, contract_hash, end_user_ref, token_hash, expires_at)
		VALUES ($1, $2, $3, 'oauth', 'oauth2', $4, 'input-user', $5, NOW() + INTERVAL '10 minutes')`, fixture.inputSessionID, bucketID, fixture.targetID, "sha256:"+strings.Repeat("0", 64), "input-"+uuid.NewString())
	execAppCredentialSourceFixture(t, ctx, pool, "seed referenced connect session", `INSERT INTO fused_connect_sessions
		(id, bucket_id, service_id, service_version_id, auth_type, auth_name, end_user_ref, state_hash, expires_at)
		VALUES ($1, $2, $3, $4, 'oauth', 'oauth2', 'connect-user', $5, NOW() + INTERVAL '10 minutes'),
		       ($6, $2, $3, $4, 'oauth', '', 'blank-connect-user', $7, NOW() + INTERVAL '10 minutes'),
		       ($8, $2, $9, $10, 'oauth', '', 'ambiguous-connect-user', $11, NOW() + INTERVAL '10 minutes')`,
		fixture.connectID, bucketID, fixture.targetID, fixture.targetVersionID, "state-"+uuid.NewString(),
		fixture.blankConnectID, "state-"+uuid.NewString(), fixture.ambiguousConnectID, fixture.ambiguousTargetID, fixture.ambiguousVersionID, "state-"+uuid.NewString())
	execAppCredentialSourceFixture(t, ctx, pool, "seed referenced auth connection", `INSERT INTO fused_auth_connections
		(id, bucket_id, service_id, service_version_id, end_user_ref, auth_type, auth_name, encrypted_dek, access_token)
		VALUES ($1, $2, $3, $4, 'grant-user', 'oauth', 'oauth2', 'dek-sentinel', 'access-sentinel'),
		       ($5, $2, $3, $4, 'blank-grant-user', 'oauth', '', 'blank-dek-sentinel', 'blank-access-sentinel'),
		       ($6, $2, $7, $8, 'ambiguous-grant-user', 'oauth', '', 'ambiguous-dek-sentinel', 'ambiguous-access-sentinel')`,
		fixture.connectionID, bucketID, fixture.targetID, fixture.targetVersionID,
		fixture.blankConnectionID, fixture.ambiguousConnectionID, fixture.ambiguousTargetID, fixture.ambiguousVersionID)
	return fixture
}

// execAppCredentialSourceFixture keeps fixture orchestration branch-free while retaining contextual SQL failures.
func execAppCredentialSourceFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, label, query string, args ...any) {
	t.Helper()
	// Every fixture write must fail immediately because later rows depend on its exact identity.
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
}

// assertAppCredentialSourceUpgrade proves every durable consumer retained the exact source before v15 state disappeared.
func assertAppCredentialSourceUpgrade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	assertNamedAppCredentialSourceUpgrade(t, ctx, pool, fixture)
	assertRequiredAuthAppCredentialSourceUpgrade(t, ctx, pool, fixture)
	assertDurableAppCredentialSources(t, ctx, pool, fixture)
	assertBlankAppCredentialSourceUpgrade(t, ctx, pool, fixture)
	assertWorkspaceAuthReferenceRetired(t, ctx, pool)
}

type appCredentialSourceProjection struct {
	authType   string
	authName   string
	authRef    string
	sourceID   uuid.UUID
	sourceType string
	sourceName string
}

// assertNamedAppCredentialSourceUpgrade verifies the existing top-level selector retains its source.
func assertNamedAppCredentialSourceUpgrade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	var got appCredentialSourceProjection
	// Filtering by service ID prevents sibling fixture selections from hiding a migration cardinality error.
	if err := pool.QueryRow(ctx, `SELECT selection->>'auth_ref', (selection->>'credential_source_service_id')::uuid,
		selection->>'credential_source_auth_type', selection->>'credential_source_auth_name'
		FROM fused_apps app CROSS JOIN LATERAL jsonb_array_elements(app.selections) selection
		WHERE app.app_id = $1 AND selection->>'service_id' = $2`, fixture.appID, fixture.targetID.String()).
		Scan(&got.authRef, &got.sourceID, &got.sourceType, &got.sourceName); err != nil {
		t.Fatalf("read migrated app selection: %v", err)
	}
	want := appCredentialSourceProjection{authRef: "${bucket.auth.gmail.oauth2}", sourceID: fixture.sourceID, sourceType: "oauth", sourceName: "oauth2"}
	// Struct equality keeps the exact source invariant visible without a high-complexity boolean chain.
	if got != want {
		t.Fatalf("migrated app source = %#v, want %#v", got, want)
	}
}

// assertDurableAppCredentialSources verifies input, callback, and grant rows share the migrated source.
func assertDurableAppCredentialSources(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	for table, id := range map[string]uuid.UUID{
		"fused_connect_input_sessions": fixture.inputSessionID,
		"fused_connect_sessions":       fixture.connectID,
		"fused_auth_connections":       fixture.connectionID,
	} {
		var got appCredentialSourceProjection
		query := `SELECT credential_source_service_id, credential_source_auth_type, credential_source_auth_name FROM ` + table + ` WHERE id = $1`
		// Every durable consumer must expose the same exact source projection after the table is retired.
		if err := pool.QueryRow(ctx, query, id).Scan(&got.sourceID, &got.sourceType, &got.sourceName); err != nil {
			t.Fatalf("read migrated %s source: %v", table, err)
		}
		want := appCredentialSourceProjection{sourceID: fixture.sourceID, sourceType: "oauth", sourceName: "oauth2"}
		// A compact projection prevents assertion branching from obscuring table parity.
		if got != want {
			t.Fatalf("migrated %s source = %#v, want %#v", table, got, want)
		}
	}
}

// assertWorkspaceAuthReferenceRetired proves v17 drops the obsolete global edge after every copy succeeds.
func assertWorkspaceAuthReferenceRetired(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var referenceTableExists bool
	// A query error or surviving table both violate the forward-only retirement boundary.
	if err := pool.QueryRow(ctx, `SELECT to_regclass('fused_workspace_auth_references') IS NOT NULL`).Scan(&referenceTableExists); err != nil || referenceTableExists {
		t.Fatalf("workspace reference table exists=%t err=%v", referenceTableExists, err)
	}
}

// assertRequiredAuthAppCredentialSourceUpgrade proves unique required-auth recovery and ambiguous fail-closed behavior.
func assertRequiredAuthAppCredentialSourceUpgrade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	assertUniqueRequiredAuthAppCredentialSource(t, ctx, pool, fixture)
	assertAmbiguousRequiredAuthAppCredentialSource(t, ctx, pool, fixture)
}

// assertUniqueRequiredAuthAppCredentialSource verifies one required reference becomes explicit runtime identity.
func assertUniqueRequiredAuthAppCredentialSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	var got appCredentialSourceProjection
	// The unique required family must project every field needed by runtime source validation.
	if err := pool.QueryRow(ctx, `SELECT selection->>'auth_type', selection->>'auth_name', selection->>'auth_ref',
		(selection->>'credential_source_service_id')::uuid, selection->>'credential_source_auth_type', selection->>'credential_source_auth_name'
		FROM fused_apps app CROSS JOIN LATERAL jsonb_array_elements(app.selections) selection
		WHERE app.app_id = $1 AND selection->>'service_id' = $2`, fixture.appID, fixture.requiredTargetID.String()).
		Scan(&got.authType, &got.authName, &got.authRef, &got.sourceID, &got.sourceType, &got.sourceName); err != nil {
		t.Fatalf("read required-auth migrated app selection: %v", err)
	}
	want := appCredentialSourceProjection{
		authType: "oauth", authName: "oauth2", authRef: "${bucket.auth.gmail.oauth2}",
		sourceID: fixture.sourceID, sourceType: "oauth", sourceName: "oauth2",
	}
	// One referenced required family becomes the selection's explicit immutable routing identity.
	if got != want {
		t.Fatalf("required-auth migration = %#v, want %#v", got, want)
	}
}

type ambiguousAppCredentialSourceProjection struct {
	hasAuthRef    bool
	hasSourceID   bool
	hasSourceType bool
	hasSourceName bool
	requiredCount int
}

// assertAmbiguousRequiredAuthAppCredentialSource verifies multiple references remain unbound.
func assertAmbiguousRequiredAuthAppCredentialSource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	var got ambiguousAppCredentialSourceProjection
	// Boolean key checks distinguish a fail-closed omission from misleading empty routing fields.
	if err := pool.QueryRow(ctx, `SELECT selection ? 'auth_ref', selection ? 'credential_source_service_id',
		selection ? 'credential_source_auth_type', selection ? 'credential_source_auth_name',
		jsonb_array_length(selection->'required_auth')
		FROM fused_apps app CROSS JOIN LATERAL jsonb_array_elements(app.selections) selection
		WHERE app.app_id = $1 AND selection->>'service_id' = $2`, fixture.appID, fixture.ambiguousTargetID.String()).
		Scan(&got.hasAuthRef, &got.hasSourceID, &got.hasSourceType, &got.hasSourceName, &got.requiredCount); err != nil {
		t.Fatalf("read ambiguous required-auth app selection: %v", err)
	}
	want := ambiguousAppCredentialSourceProjection{requiredCount: 2}
	// Multiple referenced required families preserve the reviewed requirements but never guess one credential source.
	if got != want {
		t.Fatalf("ambiguous required-auth migration = %#v, want %#v", got, want)
	}
}

// assertBlankAppCredentialSourceUpgrade verifies unique pinned schemes recover while ambiguous historical rows stay inactive.
func assertBlankAppCredentialSourceUpgrade(t *testing.T, ctx context.Context, pool *pgxpool.Pool, fixture appCredentialSourceUpgradeFixture) {
	t.Helper()
	assertBlankCredentialSourceRows(t, ctx, pool, map[string]uuid.UUID{
		"fused_connect_sessions": fixture.blankConnectID,
		"fused_auth_connections": fixture.blankConnectionID,
	}, appCredentialSourceProjection{authName: "oauth2", sourceID: fixture.sourceID, sourceType: "oauth", sourceName: "oauth2"})
	assertBlankCredentialSourceRows(t, ctx, pool, map[string]uuid.UUID{
		"fused_connect_sessions": fixture.ambiguousConnectID,
		"fused_auth_connections": fixture.ambiguousConnectionID,
	}, appCredentialSourceProjection{sourceID: fixture.ambiguousTargetID, sourceType: "oauth"})
}

// assertBlankCredentialSourceRows compares one expected source projection across equivalent durable tables.
func assertBlankCredentialSourceRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rows map[string]uuid.UUID, want appCredentialSourceProjection) {
	t.Helper()
	for table, id := range rows {
		var got appCredentialSourceProjection
		// The shared column projection keeps session and grant assertions behaviorally identical.
		if err := pool.QueryRow(ctx, `SELECT auth_name, credential_source_service_id, credential_source_auth_type, credential_source_auth_name FROM `+table+` WHERE id = $1`, id).
			Scan(&got.authName, &got.sourceID, &got.sourceType, &got.sourceName); err != nil {
			t.Fatalf("read blank %s: %v", table, err)
		}
		// Struct equality verifies unique recovery and ambiguous inactivity without branching per field.
		if got != want {
			t.Fatalf("blank %s = %#v, want %#v", table, got, want)
		}
	}
}
