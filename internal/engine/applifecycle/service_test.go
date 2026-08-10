package applifecycle

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

// testService creates a Service backed by a real Postgres pool for integration
// tests. Tests are skipped in short mode or when no database is available.
func testService(t *testing.T) *Service {
	t.Helper()
	db := testDB(t)
	cleanAppTables(t, db)
	return New(store.NewPostgresStore(db))
}

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL is required for destructive lifecycle integration tests")
	}
	parsed, err := url.Parse(connStr)
	if err != nil || !strings.Contains(strings.ToLower(parsed.Path), "test") {
		t.Fatal("TEST_DATABASE_URL must name a dedicated test database")
	}
	pool, err := pgxpool.New(context.Background(), connStr)
	if err != nil {
		t.Fatalf("connect to TEST_DATABASE_URL: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping TEST_DATABASE_URL: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func cleanAppTables(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// Order matters due to FK constraints.
	for _, q := range []string{
		`DELETE FROM fused_app_tombstones`,
		`DELETE FROM fused_app_tokens`,
		`DELETE FROM fused_app_family_buckets`,
		`DELETE FROM fused_app_capabilities`,
		`DELETE FROM fused_apps`,
		`DELETE FROM fused_app_families`,
	} {
		_, err := db.Exec(ctx, q)
		require.NoError(t, err, "clean table: %s", q)
	}
}

// --- Tests ---

func TestCreateOrGetFamily_Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	accountID := uuid.New()
	params := CreateFamilyParams{
		AccountID:      accountID,
		Kind:           "sdk",
		CanonicalName:  "my-test-sdk",
		DisplayName:    "My Test SDK",
		TargetLanguage: "typescript",
		OwnerSubjectID: uuid.New(),
	}

	// First call creates.
	f1, created1, err := svc.CreateOrGetFamily(ctx, params)
	require.NoError(t, err)
	assert.True(t, created1)
	assert.Equal(t, "my-test-sdk", f1.CanonicalName)

	// Second call with same identity returns existing.
	f2, created2, err := svc.CreateOrGetFamily(ctx, params)
	require.NoError(t, err)
	assert.False(t, created2)
	assert.Equal(t, f1.AppFamilyID, f2.AppFamilyID)
}

func TestPublishVersion_IdempotentSameSource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "test-sdk", "typescript")

	selections := []byte(`[{"service":"test"}]`)
	sourceHash := "abc123"
	params := PublishVersionParams{
		AppFamilyID:        family.AppFamilyID,
		AccountID:          family.AccountID,
		AppID:              uuid.New(),
		Kind:               "sdk",
		Version:            "1.0.0",
		ConfigKey:          "sdk:test-sdk:1.0.0",
		SourceHash:         sourceHash,
		Selections:         selections,
		ScopeSchemaVersion: 1,
		GeneratorVersion:   "v1.2.3",
		CreatedBy:          family.OwnerSubjectID,
	}

	// First publish creates.
	r1, err := svc.PublishVersion(ctx, params)
	require.NoError(t, err)
	assert.True(t, r1.Created)
	assert.Equal(t, "active", r1.App.Status)

	// Second publish with same params is a no-op.
	r2, err := svc.PublishVersion(ctx, params)
	require.NoError(t, err)
	assert.False(t, r2.Created)
	assert.True(t, r2.NoOp)
}

func TestPublishVersion_ImmutableVersionFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "immutable-test", "typescript")

	appID := uuid.New()
	params := PublishVersionParams{
		AppFamilyID:        family.AppFamilyID,
		AccountID:          family.AccountID,
		AppID:              appID,
		Kind:               "sdk",
		Version:            "1.0.0",
		ConfigKey:          "sdk:immutable-test:1.0.0",
		SourceHash:         "hash-v1",
		Selections:         []byte(`[{"service":"a"}]`),
		ScopeSchemaVersion: 1,
		GeneratorVersion:   "v1.0.0",
		CreatedBy:          family.OwnerSubjectID,
	}

	// First publish succeeds.
	_, err := svc.PublishVersion(ctx, params)
	require.NoError(t, err)

	// The source hash is only one part of immutable identity; changing the
	// executable scope under the same hash must also be rejected.
	params.Selections = []byte(`[{"select_all":true}]`)
	_, err = svc.PublishVersion(ctx, params)
	assert.ErrorIs(t, err, ErrAppVersionImmutable)
	params.Selections = []byte(`[{"service":"a"}]`)

	// Same version, different source — must fail.
	params.SourceHash = "hash-v2-different"
	_, err = svc.PublishVersion(ctx, params)
	assert.ErrorIs(t, err, ErrAppVersionImmutable)
}

func TestPublishVersion_KindMustMatchFamily(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	family := createTestFamily(t, svc, context.Background(), store.AppKindSDK, "kind-test", "typescript")
	_, err := svc.PublishVersion(context.Background(), PublishVersionParams{
		AppFamilyID: family.AppFamilyID, AccountID: family.AccountID, AppID: uuid.New(),
		Kind: store.AppKindMCP, Version: "1.0.0", ConfigKey: "mcp:kind-test:1.0.0",
		SourceHash: "source", Selections: []byte(`[]`), ScopeSchemaVersion: 1,
	})
	assert.ErrorIs(t, err, store.ErrAppFamilyKindMismatch)
}

func TestPublishVersion_DifferentVersionsOK(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "multi-version", "typescript")

	base := PublishVersionParams{
		AppFamilyID:        family.AppFamilyID,
		AccountID:          family.AccountID,
		Kind:               "sdk",
		ConfigKey:          "sdk:multi-version:1.0.0",
		SourceHash:         "hash",
		Selections:         []byte(`[{"service":"a"}]`),
		ScopeSchemaVersion: 1,
		GeneratorVersion:   "v1.0.0",
		CreatedBy:          family.OwnerSubjectID,
	}

	// Publish v1.0.0
	v1 := base
	v1.AppID = uuid.New()
	v1.Version = "1.0.0"
	v1.ConfigKey = "sdk:multi-version:1.0.0"
	r1, err := svc.PublishVersion(ctx, v1)
	require.NoError(t, err)
	assert.True(t, r1.Created)

	// Publish v2.0.0 — same capability, different version, allowed.
	v2 := base
	v2.AppID = uuid.New()
	v2.Version = "2.0.0"
	v2.ConfigKey = "sdk:multi-version:2.0.0"
	v2.SourceHash = "hash-v2"
	r2, err := svc.PublishVersion(ctx, v2)
	require.NoError(t, err)
	assert.True(t, r2.Created)

	// Both versions exist.
	apps, err := svc.store.(store.Store).ListApps(ctx, family.AppFamilyID)
	require.NoError(t, err)
	assert.Len(t, apps, 2)
}

func TestDeprecateAndUndeprecate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "mcp", "deprecate-test", "")
	app := createTestApp(t, svc, ctx, family, "1.0.0")

	// Deprecate
	err := svc.Deprecate(ctx, app.AppID, "Use v2.0.0 instead", nil)
	require.NoError(t, err)

	got, err := svc.store.GetApp(ctx, app.AppID)
	require.NoError(t, err)
	assert.Equal(t, "deprecated", got.Status)
	assert.Equal(t, "Use v2.0.0 instead", got.DeprecationMessage)

	// Undeprecate
	err = svc.Undeprecate(ctx, app.AppID)
	require.NoError(t, err)

	got, err = svc.store.GetApp(ctx, app.AppID)
	require.NoError(t, err)
	assert.Equal(t, "active", got.Status)
	assert.Empty(t, got.DeprecationMessage)
}

func TestDeactivate_Irreversible(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "mcp", "deactivate-test", "")
	app := createTestApp(t, svc, ctx, family, "1.0.0")

	actorID := uuid.New()
	err := svc.Deactivate(ctx, app.AppID, actorID)
	require.NoError(t, err)

	// App is gone.
	_, err = svc.store.GetApp(ctx, app.AppID)
	assert.ErrorIs(t, err, store.ErrAppNotFound)

	// Tombstone exists.
	exists, err := svc.store.(store.Store).AppTombstoneExists(ctx, family.AppFamilyID, "1.0.0")
	require.NoError(t, err)
	assert.True(t, exists)

	// Cannot republish the same version.
	_, err = svc.PublishVersion(ctx, PublishVersionParams{
		AppFamilyID:        family.AppFamilyID,
		AccountID:          family.AccountID,
		AppID:              uuid.New(),
		Kind:               "mcp",
		Version:            "1.0.0",
		ConfigKey:          "mcp:deactivate-test:1.0.0",
		SourceHash:         "new-hash",
		Selections:         []byte(`[]`),
		ScopeSchemaVersion: 1,
		CreatedBy:          family.OwnerSubjectID,
	})
	assert.ErrorIs(t, err, ErrAppTombstoneExists)
}

func TestRuntimeTokenAuthorization_Success(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "auth-test", "typescript")
	app := createTestApp(t, svc, ctx, family, "1.0.0")

	// Generate a token.
	plaintext, _, err := svc.GenerateToken(ctx, GenerateTokenParams{AppFamilyID: family.AppFamilyID, Name: "default"})
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)

	// Exercise the validator used by SDK and MCP execution rather than a
	// lifecycle-only wrapper that production never calls.
	identity, err := auth.NewTokenValidator(svc.store.(store.Store)).Validate(ctx, app.AppID, plaintext)
	require.NoError(t, err)
	assert.Equal(t, app.AppID, identity.AppID)
	assert.Equal(t, family.AppFamilyID, identity.AppFamilyID)
	assert.Equal(t, store.AppStatusActive, identity.Status)
}

func TestRuntimeTokenAuthorization_DeprecatedAppStillAuthorized(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "mcp", "depr-auth-test", "")
	app := createTestApp(t, svc, ctx, family, "1.0.0")

	plaintext, _, err := svc.GenerateToken(ctx, GenerateTokenParams{AppFamilyID: family.AppFamilyID, Name: "default"})
	require.NoError(t, err)

	// Deprecate the app.
	err = svc.Deprecate(ctx, app.AppID, "Old version", nil)
	require.NoError(t, err)

	// Authorization still succeeds for deprecated apps.
	identity, err := auth.NewTokenValidator(svc.store.(store.Store)).Validate(ctx, app.AppID, plaintext)
	require.NoError(t, err)
	assert.Equal(t, store.AppStatusDeprecated, identity.Status)
}

func TestRuntimeTokenAuthorization_DeactivatedAppDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "mcp", "dead-auth-test", "")
	app := createTestApp(t, svc, ctx, family, "1.0.0")

	plaintext, _, err := svc.GenerateToken(ctx, GenerateTokenParams{AppFamilyID: family.AppFamilyID, Name: "default"})
	require.NoError(t, err)

	// Deactivate the app.
	err = svc.Deactivate(ctx, app.AppID, uuid.New())
	require.NoError(t, err)

	// Authorization must fail.
	_, err = auth.NewTokenValidator(svc.store.(store.Store)).Validate(ctx, app.AppID, plaintext)
	assert.ErrorIs(t, err, auth.ErrUnauthorized)
}

func TestRuntimeTokenAuthorization_WrongFamilyDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	// Two families — tokens from family A should not authorize apps in family B.
	familyA := createTestFamily(t, svc, ctx, "sdk", "family-a", "typescript")
	appA := createTestApp(t, svc, ctx, familyA, "1.0.0")

	familyB := createTestFamily(t, svc, ctx, "sdk", "family-b", "typescript")
	appB := createTestApp(t, svc, ctx, familyB, "1.0.0")

	plaintextA, _, err := svc.GenerateToken(ctx, GenerateTokenParams{AppFamilyID: familyA.AppFamilyID, Name: "token-a"})
	require.NoError(t, err)

	// Token from family A with app from family B must fail.
	_, err = auth.NewTokenValidator(svc.store.(store.Store)).Validate(ctx, appB.AppID, plaintextA)
	assert.ErrorIs(t, err, auth.ErrUnauthorized)
	_ = appA // used above
}

func TestCapabilityExpansion_NewFamily(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "cap-test", "typescript")

	// No apps yet — no expansion.
	result, err := svc.CapabilityExpansion(ctx, family.AppFamilyID, []byte("[]"))
	require.NoError(t, err)
	assert.False(t, result.Expands)
}

func TestCapabilityExpansion_DifferentHash(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "cap-expand", "typescript")
	serviceID, versionID := uuid.New(), uuid.New()
	existing := capabilitySelections(t, serviceID, versionID, "list")
	createTestAppWithSelections(t, svc, ctx, family, "1.0.0", existing)

	expanded := capabilitySelections(t, serviceID, versionID, "list", "create")
	result, err := svc.CapabilityExpansion(ctx, family.AppFamilyID, expanded)
	require.NoError(t, err)
	assert.True(t, result.Expands)
}

func TestCapabilityExpansion_ContractionIsNotExpansion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	svc := testService(t)
	ctx := context.Background()
	family := createTestFamily(t, svc, ctx, "sdk", "cap-contract", "typescript")
	serviceID, versionID := uuid.New(), uuid.New()
	createTestAppWithSelections(t, svc, ctx, family, "1.0.0",
		capabilitySelections(t, serviceID, versionID, "list", "create"))

	result, err := svc.CapabilityExpansion(ctx, family.AppFamilyID,
		capabilitySelections(t, serviceID, versionID, "list"))
	require.NoError(t, err)
	assert.False(t, result.Expands)
}

func TestGenerateToken_PlaintextReturnedOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	db := testDB(t)
	cleanAppTables(t, db)
	svc := New(store.NewPostgresStore(db))
	ctx := context.Background()

	family := createTestFamily(t, svc, ctx, "sdk", "token-test", "typescript")

	plaintext, tok, err := svc.GenerateToken(ctx, GenerateTokenParams{AppFamilyID: family.AppFamilyID, Name: "my-token"})
	require.NoError(t, err)
	assert.Contains(t, plaintext, "fused-app-")
	assert.Equal(t, "my-token", tok.Name)

	// Listing returns metadata only. Hashes remain inside the database and the
	// authorization query so a metadata surface cannot accidentally expose them.
	tokens, err := svc.store.(store.Store).ListAppTokens(ctx, family.AppFamilyID)
	require.NoError(t, err)
	assert.Len(t, tokens, 1)
	assert.Equal(t, tok.ID, tokens[0].ID)
	assert.Equal(t, tok.Name, tokens[0].Name)

	var storedHash string
	require.NoError(t, db.QueryRow(ctx, `
		SELECT token_hash FROM fused_app_tokens WHERE id = $1
	`, tok.ID).Scan(&storedHash))
	assert.Equal(t, auth.HashToken(plaintext), storedHash)
}

// --- helpers ---

func createTestFamily(t *testing.T, svc *Service, ctx context.Context, kind store.AppKind, name, targetLanguage string) *store.AppFamily {
	t.Helper()
	accountID := uuid.New()
	family, created, err := svc.CreateOrGetFamily(ctx, CreateFamilyParams{
		AccountID:      accountID,
		Kind:           kind,
		CanonicalName:  name,
		DisplayName:    name,
		TargetLanguage: targetLanguage,
		OwnerSubjectID: uuid.New(),
	})
	require.NoError(t, err)
	require.True(t, created)
	return family
}

func createTestApp(t *testing.T, svc *Service, ctx context.Context, family *store.AppFamily, version string) store.App {
	t.Helper()
	return createTestAppWithSelections(t, svc, ctx, family, version, []byte("[]"))
}

func createTestAppWithSelections(t *testing.T, svc *Service, ctx context.Context, family *store.AppFamily, version string, selections []byte) store.App {
	t.Helper()
	params := PublishVersionParams{
		AppFamilyID:        family.AppFamilyID,
		AccountID:          family.AccountID,
		AppID:              uuid.New(),
		Kind:               family.Kind,
		Version:            version,
		ConfigKey:          family.Kind.String() + ":" + family.CanonicalName + ":" + version,
		SourceHash:         "src-" + version,
		Selections:         selections,
		ScopeSchemaVersion: 1,
		GeneratorVersion:   "v1.0.0",
		CreatedBy:          family.OwnerSubjectID,
	}
	result, err := svc.PublishVersion(ctx, params)
	require.NoError(t, err)
	require.True(t, result.Created)
	return result.App
}

func capabilitySelections(t *testing.T, serviceID, versionID uuid.UUID, operations ...string) []byte {
	t.Helper()
	data, err := json.Marshal([]models.SDKSelection{{
		ServiceID: serviceID, ServiceVersionID: versionID, OperationNames: operations,
	}})
	require.NoError(t, err)
	return data
}
