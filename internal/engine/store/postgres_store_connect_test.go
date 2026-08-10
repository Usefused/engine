package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var connectAuthTestMasterKey = []byte("12345678901234567890123456789012")

func TestPostgresStore_BucketAttachedConnectAuth(t *testing.T) {
	fixture := setupConnectAuthStore(t)

	t.Run("bucket names resolve in one exact batch", func(t *testing.T) {
		testGetBucketsByNames(t, fixture)
	})

	t.Run("config is upserted only for a bucket in the workspace", func(t *testing.T) {
		testConnectConfigOwnership(t, fixture)
	})

	t.Run("connections are reusable by bucket and isolated across buckets", func(t *testing.T) {
		testAuthConnectionsReusableByBucket(t, fixture)
	})

	t.Run("connect sessions are single lookup records with cleanup", func(t *testing.T) {
		testConnectSessionLifecycle(t, fixture)
	})

	t.Run("connection resources reconcile and select without broad reads", func(t *testing.T) {
		testConnectionResourceLifecycle(t, fixture)
	})

	t.Run("workspace profile layers resolve precedence and stay version/operation scoped", func(t *testing.T) {
		testWorkspaceConnectionProfileLifecycle(t, fixture)
	})
}

// testGetBucketsByNames verifies plan-time lookup returns requested workspace
// buckets without broad listing or cross-workspace rows.
func testGetBucketsByNames(t *testing.T, f connectAuthFixture) {
	t.Helper()
	buckets, err := f.store.GetBucketsByNames(f.ctx, []string{
		"connect-auth-prod-" + f.bucketA.String(), "missing-bucket",
	})
	if err != nil {
		t.Fatalf("GetBucketsByNames: %v", err)
	}
	if len(buckets) != 1 || buckets[0].ID != f.bucketA {
		t.Fatalf("exact bucket batch = %#v", buckets)
	}
}

// testWorkspaceConnectionProfileLifecycle covers the plan's verification
// matrix for workspace-scoped profiles: baseline vs override precedence
// resolved in SQL, reset deleting only the override, version/auth-type
// isolation, and one-transaction batch reconciliation.
func testWorkspaceConnectionProfileLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	profileStore, ok := f.store.(WorkspaceProfileStore)
	if !ok {
		t.Fatal("store does not implement workspace profile store")
	}
	versionID := uuid.New()
	if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-profile", versionID, "Profile Service", f.accountID); err != nil {
		t.Fatalf("activate profile version: %v", err)
	}
	baseline := seedWorkspaceProfileBaseline(t, f, profileStore, versionID)
	assertEffectiveProfileIsBaseline(t, f, profileStore, versionID, baseline)
	assertOverrideWinsOverBaseline(t, f, profileStore, versionID)
	assertWorkspaceBindingOperationScoping(t, f, versionID)
	assertWorkspaceProfileVersionAndAuthIsolation(t, f, profileStore, versionID)
	assertResetDeletesOnlyOverride(t, f, profileStore, versionID, baseline)
	assertBatchWorkspaceProfileReconcile(t, f, profileStore)
}

// seedWorkspaceProfileBaseline attaches the pinned Registry/Fused baseline
// layer -- this mirrors what activation does, independent of any bucket.
func seedWorkspaceProfileBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) WorkspaceConnectionProfile {
	t.Helper()
	registryProfileID := uuid.New()
	basePath := "base_url"
	revision := 1
	baseline := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		AuthType: "oauth", Layer: "baseline", RegistryProfileID: &registryProfileID,
		ProfileRevision: revision, ProfileHash: "baseline-hash-1", Provenance: "provider",
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	binding := WorkspaceConnectionBinding{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		SourceKind: "connection_resource", SourcePath: &basePath, TargetLocation: "base_url",
		Mode: "force", Provenance: "provider", SourceProfileRevision: &revision,
	}
	batchStore, ok := f.store.(WorkspaceProfileBatchStore)
	if !ok {
		t.Fatal("store does not implement workspace profile batch reconciliation")
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, []WorkspaceProfileReplacement{{Profile: baseline, Bindings: []WorkspaceConnectionBinding{binding}}}, nil); err != nil {
		t.Fatalf("seed baseline via reconcile: %v", err)
	}
	stored, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || stored == nil || stored.Layer != "baseline" {
		t.Fatalf("seeded baseline = %#v, err=%v", stored, err)
	}
	return *stored
}

// assertEffectiveProfileIsBaseline proves the effective read falls back to
// the baseline when no override row exists for the tuple.
func assertEffectiveProfileIsBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID, baseline WorkspaceConnectionProfile) {
	t.Helper()
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "baseline" || effective.ID != baseline.ID {
		t.Fatalf("effective profile without override = %#v, err=%v", effective, err)
	}
}

// assertOverrideWinsOverBaseline proves the SQL-resolved effective read
// prefers the override once one exists, without disturbing the baseline row.
func assertOverrideWinsOverBaseline(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) WorkspaceConnectionProfile {
	t.Helper()
	literal := "v1"
	override := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		AuthType: "oauth", ProfileRevision: 1, ProfileHash: "override-hash-1",
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	stored, err := profileStore.UpsertWorkspaceProfileOverride(f.ctx, override, []WorkspaceConnectionBinding{{
		ServiceID: f.serviceID, ServiceVersionID: versionID,
		SourceKind: "literal", LiteralValue: &literal, TargetLocation: "header", TargetName: "X-Version",
		Mode: "force", Provenance: "workspace", OperationIDs: []string{"getIssue"},
	}})
	if err != nil {
		t.Fatalf("UpsertWorkspaceProfileOverride: %v", err)
	}
	if stored.Layer != "override" || stored.Provenance != "workspace" || stored.RegistryProfileID != nil {
		t.Fatalf("stored override = %#v", stored)
	}
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "override" || effective.ID != stored.ID {
		t.Fatalf("effective profile with override present = %#v, err=%v", effective, err)
	}
	bindings, err := profileStore.ListWorkspaceProfileBindings(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || len(bindings) != 1 || bindings[0].LiteralValue == nil || *bindings[0].LiteralValue != literal {
		t.Fatalf("override bindings = %#v, err=%v", bindings, err)
	}
	return *stored
}

// assertWorkspaceBindingOperationScoping proves the execution-binding read
// filters by operation without loading the whole profile's binding set.
func assertWorkspaceBindingOperationScoping(t *testing.T, f connectAuthFixture, versionID uuid.UUID) {
	t.Helper()
	execStore, ok := f.store.(WorkspaceProfileStore)
	if !ok {
		t.Fatal("store does not implement workspace profile store")
	}
	bindings, err := execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, f.serviceID, versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 1 || bindings[0].TargetName != "X-Version" {
		t.Fatalf("getIssue-scoped bindings = %#v, err=%v", bindings, err)
	}
	bindings, err = execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, f.serviceID, versionID, "oauth", "otherOperation")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("expected operation-scoped binding to be excluded for a different operation, got %#v, err=%v", bindings, err)
	}
	// A different bucket in the same workspace resolves the same effective
	// bindings -- profiles are workspace-scoped, not bucket-scoped.
	bindings, err = execStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketB, f.serviceID, versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 1 {
		t.Fatalf("expected sibling bucket in the same workspace to resolve the same bindings, got %#v, err=%v", bindings, err)
	}
}

// assertWorkspaceProfileVersionAndAuthIsolation proves a profile for one
// version/auth family never leaks into a lookup for another.
func assertWorkspaceProfileVersionAndAuthIsolation(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID) {
	t.Helper()
	otherVersion, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, uuid.New(), "oauth")
	if err != nil || otherVersion != nil {
		t.Fatalf("unrelated version leaked a profile: %#v, err=%v", otherVersion, err)
	}
	otherAuth, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oidc")
	if err != nil || otherAuth != nil {
		t.Fatalf("unrelated auth family leaked a profile: %#v, err=%v", otherAuth, err)
	}
	bindings, err := profileStore.ListWorkspaceBindingsForExecution(f.ctx, f.bucketA, uuid.New(), versionID, "oauth", "getIssue")
	if err != nil || len(bindings) != 0 {
		t.Fatalf("cross-service bindings = %#v, err=%v", bindings, err)
	}
}

// assertResetDeletesOnlyOverride is the "override survives" analog under the
// new model: resetting removes only the override row so the baseline --
// which was never touched -- immediately becomes the effective profile
// again, with its original revision/hash intact and no Registry call needed.
func assertResetDeletesOnlyOverride(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore, versionID uuid.UUID, baseline WorkspaceConnectionProfile) {
	t.Helper()
	if err := profileStore.ResetWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth"); err != nil {
		t.Fatalf("ResetWorkspaceProfile: %v", err)
	}
	effective, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || effective == nil || effective.Layer != "baseline" || effective.ID != baseline.ID || effective.ProfileHash != baseline.ProfileHash {
		t.Fatalf("effective profile after reset = %#v, err=%v, want unchanged baseline %#v", effective, err, baseline)
	}
	bindings, err := profileStore.ListWorkspaceProfileBindings(f.ctx, f.serviceID, versionID, "oauth")
	if err != nil || len(bindings) != 1 || bindings[0].TargetLocation != "base_url" {
		t.Fatalf("bindings after reset should be the baseline's own rows, got %#v, err=%v", bindings, err)
	}
	// Resetting again is idempotent: no override remains to delete.
	if err := profileStore.ResetWorkspaceProfile(f.ctx, f.serviceID, versionID, "oauth"); err != nil {
		t.Fatalf("idempotent ResetWorkspaceProfile: %v", err)
	}
}

// assertBatchWorkspaceProfileReconcile proves multi-version replacements and
// exact deletes use the fixed-query transactional store path in one
// transaction, matching set-based reconciliation used by workspace apply.
func assertBatchWorkspaceProfileReconcile(t *testing.T, f connectAuthFixture, profileStore WorkspaceProfileStore) {
	t.Helper()
	batchStore, ok := f.store.(WorkspaceProfileBatchStore)
	if !ok {
		t.Fatal("store does not implement workspace profile batch reconciliation")
	}
	firstVersion, secondVersion := uuid.New(), uuid.New()
	for _, versionID := range []uuid.UUID{firstVersion, secondVersion} {
		if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-batch-"+versionID.String(), versionID, "Batch Service", f.accountID); err != nil {
			t.Fatalf("activate batch version: %v", err)
		}
	}
	first := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: firstVersion,
		AuthType: "oauth", Layer: "override", Provenance: "workspace",
		ProfileRevision: 1, ProfileHash: "batch-1", ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	literal := "batch"
	second := WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: secondVersion,
		AuthType: "oauth", Layer: "override", Provenance: "workspace",
		ProfileRevision: 1, ProfileHash: "batch-2", ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}
	binding := WorkspaceConnectionBinding{
		ServiceID: f.serviceID, ServiceVersionID: secondVersion,
		SourceKind: "literal", LiteralValue: &literal, TargetLocation: "query", TargetName: "portal",
		Mode: "force", Provenance: "workspace",
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, []WorkspaceProfileReplacement{
		{Profile: first, Bindings: nil}, {Profile: second, Bindings: []WorkspaceConnectionBinding{binding}},
	}, nil); err != nil {
		t.Fatalf("batch replace profiles: %v", err)
	}
	profiles, err := profileStore.GetEffectiveWorkspaceProfiles(f.ctx, []WorkspaceProfileRef{
		{ServiceID: f.serviceID, ServiceVersionID: firstVersion, AuthType: "oauth"},
		{ServiceID: f.serviceID, ServiceVersionID: secondVersion, AuthType: "oauth"},
	})
	if err != nil || len(profiles) != 2 {
		t.Fatalf("batch profile lookup = %#v, err=%v", profiles, err)
	}
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, nil, []WorkspaceProfileRef{
		{ServiceID: f.serviceID, ServiceVersionID: firstVersion, AuthType: "oauth"},
	}); err != nil {
		t.Fatalf("batch delete profile: %v", err)
	}
	removed, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, firstVersion, "oauth")
	if err != nil || removed != nil {
		t.Fatalf("batch profile delete left %#v, err=%v", removed, err)
	}
	kept, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, secondVersion, "oauth")
	if err != nil || kept == nil {
		t.Fatalf("batch delete removed an unrelated version: %#v, err=%v", kept, err)
	}
}

// testConnectionResourceLifecycle covers authoritative batch replacement,
// automatic sole default, explicit default, and connection-scoped lookup.
func testConnectionResourceLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	connection := upsertOAuthConnectionForUser(t, f, "resource_user")
	first := []ConnectionResource{{
		ConnectionID: connection.ID, BucketID: f.bucketA, ServiceID: f.serviceID,
		ProviderResourceID: "cloud-a", ResourceType: "jira_site", DisplayName: "Acme",
		BaseURL: "https://api.atlassian.com/ex/jira/cloud-a", MetadataJSON: []byte(`{"provider_resource_id":"cloud-a"}`),
	}}
	resources, err := f.store.ReconcileConnectionResources(f.ctx, connection.ID, first)
	if err != nil || len(resources) != 1 || !resources[0].IsDefault {
		t.Fatalf("first reconcile: resources=%#v err=%v", resources, err)
	}
	second := append(first, ConnectionResource{
		ConnectionID: connection.ID, BucketID: f.bucketA, ServiceID: f.serviceID,
		ProviderResourceID: "cloud-b", ResourceType: "jira_site", DisplayName: "Beta",
		BaseURL: "https://api.atlassian.com/ex/jira/cloud-b", MetadataJSON: []byte(`{}`),
	})
	resources, err = f.store.ReconcileConnectionResources(f.ctx, connection.ID, second)
	if err != nil || len(resources) != 2 {
		t.Fatalf("second reconcile: resources=%#v err=%v", resources, err)
	}
	selected, count, err := f.store.GetConnectionResourceForExecution(f.ctx, connection.ID, nil)
	if err != nil || selected == nil || selected.ProviderResourceID != "cloud-a" || count != 2 {
		t.Fatalf("default selection: selected=%#v count=%d err=%v", selected, count, err)
	}
	if _, err := f.store.SetDefaultConnectionResource(f.ctx, connection.ID, resources[1].ID); err != nil {
		t.Fatalf("SetDefaultConnectionResource: %v", err)
	}
	selected, _, err = f.store.GetConnectionResourceForExecution(f.ctx, connection.ID, nil)
	if err != nil || selected == nil || selected.ID != resources[1].ID {
		t.Fatalf("updated default selection: selected=%#v err=%v", selected, err)
	}
	resources, err = f.store.ReconcileConnectionResources(f.ctx, connection.ID, second[1:])
	if err != nil || len(resources) != 1 || resources[0].ProviderResourceID != "cloud-b" || !resources[0].IsDefault {
		t.Fatalf("authoritative removal: resources=%#v err=%v", resources, err)
	}
}

// upsertOAuthConnectionForUser creates an isolated connection key so resource
// tests do not depend on state created by sibling subtests.
func upsertOAuthConnectionForUser(t *testing.T, f connectAuthFixture, endUserRef string) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "resource-access")
	connection, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID: f.bucketA, ServiceID: f.serviceID,
		EndUserRef: endUserRef, AuthType: "oauth", AuthName: "oauth", EncryptedDEK: encrypted.dek,
		EncryptedAccessToken: encrypted.values[0], TokenType: "Bearer", RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection resource user: %v", err)
	}
	return connection
}

func TestPostgresStoreConnectRejectsPlaintextAuthMaterial(t *testing.T) {
	s := &postgresStore{}
	_, err := s.UpsertConnectConfig(context.Background(), ConnectConfig{
		EncryptedDEK:          "dek",
		EncryptedClientID:     "client-id",
		EncryptedClientSecret: "client-secret",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected config plaintext rejection, got %v", err)
	}

	_, err = s.UpsertAuthConnection(context.Background(), AuthConnection{
		EncryptedDEK:         "dek",
		EncryptedAccessToken: "access-token",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected connection plaintext rejection, got %v", err)
	}

	_, err = s.CreateConnectSession(context.Background(), ConnectSession{
		EncryptedPKCEVerifier: "pkce-verifier",
	})
	if !errors.Is(err, ErrInvalidEncryptedAuthMaterial) {
		t.Fatalf("expected session plaintext rejection, got %v", err)
	}
}

type connectAuthFixture struct {
	ctx           context.Context
	cancel        context.CancelFunc
	pool          interface{ Close() }
	store         Store
	workspaceID   uuid.UUID
	bucketA       uuid.UUID
	bucketB       uuid.UUID
	serviceID     uuid.UUID
	appID         uuid.UUID
	appFamilyID   uuid.UUID
	ownerTeamID   uuid.UUID
	accountID     uuid.UUID
	ownsWorkspace bool
}

// setupConnectAuthStore keeps fixture ownership explicit so the same test is
// safe against both disposable CI databases and a running developer Engine.
func setupConnectAuthStore(t *testing.T) connectAuthFixture {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping Connect auth store test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		cancel()
		t.Fatalf("failed to connect to DB: %v", err)
	}
	workspaceID, accountID, ownsWorkspace := connectAuthWorkspace(t, ctx, pool)
	fixture := connectAuthFixture{
		ctx:           ctx,
		cancel:        cancel,
		pool:          pool,
		store:         NewPostgresStore(pool),
		workspaceID:   workspaceID,
		bucketA:       uuid.New(),
		bucketB:       uuid.New(),
		serviceID:     uuid.New(),
		appID:         uuid.New(),
		appFamilyID:   uuid.New(),
		ownerTeamID:   seedAppOwnerTeam(t, ctx, pool),
		accountID:     accountID,
		ownsWorkspace: ownsWorkspace,
	}
	// This integration may run against a developer database, so clean up only
	// its UUID-scoped fixture instead of resetting shared Engine state.
	t.Cleanup(func() {
		cleanupConnectAuthFixture(pool, fixture)
		pool.Close()
		cancel()
	})
	seedConnectAuthFixture(t, pool, fixture)
	return fixture
}

// connectAuthWorkspace reuses the Engine singleton when present because
// creating a second workspace would violate the production schema by design.
func connectAuthWorkspace(t *testing.T, ctx context.Context, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) (uuid.UUID, uuid.UUID, bool) {
	t.Helper()
	var workspaceID, accountID uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id, account_id FROM fused_workspaces LIMIT 1`).Scan(&workspaceID, &accountID)
	if err == nil {
		return workspaceID, accountID, false
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("load connect auth workspace: %v", err)
	}
	accountID = uuid.New()
	err = pool.QueryRow(ctx, `
		INSERT INTO fused_workspaces (name, account_id, slug, singleton_key) VALUES ($1, $2, $3, 1)
		RETURNING id
	`, "Connect Auth Workspace", accountID, "connect-auth-"+uuid.NewString()).Scan(&workspaceID)
	if err != nil {
		t.Fatalf("seed connect auth workspace: %v", err)
	}
	return workspaceID, accountID, true
}

// cleanupConnectAuthFixture preserves a reused workspace while relying on
// foreign keys to remove every connection row owned by the test buckets.
func cleanupConnectAuthFixture(db execer, fixture connectAuthFixture) {
	ctx := context.Background()
	_, _ = db.Exec(ctx, `DELETE FROM fused_apps WHERE app_id = $1`, fixture.appID)
	_, _ = db.Exec(ctx, `DELETE FROM fused_app_families WHERE app_family_id = $1`, fixture.appFamilyID)
	_, _ = db.Exec(ctx, `DELETE FROM fused_teams WHERE id = $1`, fixture.ownerTeamID)
	if fixture.ownsWorkspace {
		_, _ = db.Exec(ctx, `DELETE FROM fused_workspaces WHERE id = $1`, fixture.workspaceID)
		return
	}
	_, _ = db.Exec(ctx, `DELETE FROM fused_buckets WHERE id = $1 OR id = $2`, fixture.bucketA, fixture.bucketB)
	_, _ = db.Exec(ctx, `DELETE FROM fused_workspace_services WHERE service_id = $1`, fixture.serviceID)
}

type execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// seedConnectAuthFixture creates only UUID-addressable child rows, allowing
// cleanup to preserve any singleton workspace that predated the test.
func seedConnectAuthFixture(t *testing.T, db execer, f connectAuthFixture) {
	t.Helper()
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_buckets (id, name) VALUES ($1, $3), ($2, $4)
	`, f.bucketA, f.bucketB, "connect-auth-prod-"+f.bucketA.String(), "connect-auth-staging-"+f.bucketB.String()); err != nil {
		t.Fatalf("seed connect auth buckets: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_app_families
			(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
		VALUES ($1, $2, 'sdk', $3, 'Connect Auth SDK', 'typescript', $4)
	`, f.appFamilyID, f.accountID, "connect-auth-"+f.appFamilyID.String(), f.ownerTeamID); err != nil {
		t.Fatalf("seed connect auth app family: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_apps (app_id, app_family_id, account_id, version, config_key, source_hash, status)
		VALUES ($3, $1, $2, '1.0.0', $4, 'connect-auth', 'active')
	`, f.appFamilyID, f.accountID, f.appID, "sdk:connect-auth:"+f.appFamilyID.String()); err != nil {
		t.Fatalf("seed connect auth app runtime: %v", err)
	}
	if _, err := db.Exec(f.ctx, `
		INSERT INTO fused_app_family_buckets (app_family_id, bucket_id) VALUES ($1, $2)
	`, f.appFamilyID, f.bucketA); err != nil {
		t.Fatalf("seed connect auth app bucket: %v", err)
	}
}

func testConnectConfigOwnership(t *testing.T, f connectAuthFixture) {
	t.Helper()
	cfg, err := f.store.UpsertConnectConfig(f.ctx, connectConfigForFixture(t, f))
	if err != nil {
		t.Fatalf("UpsertConnectConfig: %v", err)
	}
	if cfg.BucketID != f.bucketA || cfg.ServiceID != f.serviceID {
		t.Fatalf("unexpected connect config identity: %#v", cfg)
	}
	versionID := uuid.New()
	if err := f.store.AddWorkspaceServiceVersion(f.ctx, f.serviceID, "", "v-connect", versionID, "Connect Service", f.accountID); err != nil {
		t.Fatalf("activate connect service: %v", err)
	}
	configs, err := f.store.ListConnectConfigsForService(f.ctx, f.serviceID)
	if err != nil || len(configs) != 1 || configs[0].BucketID != f.bucketA {
		t.Fatalf("ListConnectConfigsForService: configs=%#v err=%v", configs, err)
	}
	syncReader := f.store.(interface {
		ListWorkspaceConnectConfigs(context.Context) ([]WorkspaceConnectConfig, error)
	})
	exported, err := syncReader.ListWorkspaceConnectConfigs(f.ctx)
	if err != nil || len(exported) != 1 || exported[0].BucketName == "" {
		t.Fatalf("ListWorkspaceConnectConfigs: configs=%#v err=%v", exported, err)
	}

}

func connectConfigForFixture(t *testing.T, f connectAuthFixture) ConnectConfig {
	encrypted := encryptConnectAuthValues(t, "client-id-v1", "client-secret-v1")
	return ConnectConfig{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		AuthType:              "oauth",
		AuthName:              "oauth",
		Enabled:               true,
		EncryptedDEK:          encrypted.dek,
		EncryptedClientID:     encrypted.values[0],
		EncryptedClientSecret: encrypted.values[1],
		RedirectURI:           "https://engine.example.com/connect/callback",
	}
}

func testAuthConnectionsReusableByBucket(t *testing.T, f connectAuthFixture) {
	t.Helper()
	upsertConnectConfigForSummary(t, f)
	connA := upsertOAuthConnection(t, f)
	assertAuthConnectionFailureDiagnostic(t, f, connA)
	connA = assertReconnectUpsertReplacesConnection(t, f, connA)
	connB := upsertAPIKeyConnection(t, f)
	assertDifferentBucketConnections(t, connA, connB)
	assertBucketConnectionLookup(t, f, connA)
	assertConnectionListAndRefreshQuery(t, f, connA.ID, connB.ID)
	assertBucketConnectSummary(t, f)
}

// assertAuthConnectionFailureDiagnostic proves a provider failure updates only
// sanitized metadata and leaves the connection usable until Engine says otherwise.
func assertAuthConnectionFailureDiagnostic(t *testing.T, f connectAuthFixture, connection *AuthConnection) {
	t.Helper()
	failedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := f.store.RecordAuthConnectionFailure(f.ctx, connection.ID, "provider_unauthorized", "trace-123", failedAt); err != nil {
		t.Fatalf("RecordAuthConnectionFailure: %v", err)
	}
	found, err := f.store.GetAuthConnection(f.ctx, connection.BucketID, connection.ServiceID, connection.EndUserRef, connection.AuthName)
	if err != nil {
		t.Fatalf("GetAuthConnection after diagnostic: %v", err)
	}
	if found == nil || found.RefreshState != "ok" || found.LastFailureCode != "provider_unauthorized" || found.LastFailureTraceID != "trace-123" {
		t.Fatalf("unexpected connection diagnostic: %#v", found)
	}
}

// assertReconnectUpsertReplacesConnection proves callback storage for the same
// bucket/service/user restores one existing row rather than creating a second.
func assertReconnectUpsertReplacesConnection(t *testing.T, f connectAuthFixture, previous *AuthConnection) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "reconnected-access", "reconnected-refresh")
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	reconnected, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID: f.bucketA, ServiceID: f.serviceID,
		EndUserRef: "user_123", CreatedByAppID: f.appID, AuthType: "oauth", AuthName: "oauth",
		EncryptedDEK: encrypted.dek, EncryptedAccessToken: encrypted.values[0],
		EncryptedRefreshToken: encrypted.values[1], TokenType: "Bearer", ExpiresAt: &expiresAt, RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection reconnect: %v", err)
	}
	if reconnected.ID != previous.ID || reconnected.RefreshState != "ok" {
		t.Fatalf("reconnect must replace existing row and reset state: before=%#v after=%#v", previous, reconnected)
	}
	if reconnected.LastFailureCode != "" || reconnected.LastFailureAt != nil || reconnected.LastFailureTraceID != "" {
		t.Fatalf("reconnect must clear stale diagnostics: %#v", reconnected)
	}
	if decryptConnectAuthValue(t, reconnected.EncryptedDEK, reconnected.EncryptedAccessToken) != "reconnected-access" {
		t.Fatalf("reconnect did not replace encrypted access token")
	}
	return reconnected
}

func upsertConnectConfigForSummary(t *testing.T, f connectAuthFixture) {
	t.Helper()
	if _, err := f.store.UpsertConnectConfig(f.ctx, connectConfigForFixture(t, f)); err != nil {
		t.Fatalf("UpsertConnectConfig for summary: %v", err)
	}
}

func upsertOAuthConnection(t *testing.T, f connectAuthFixture) *AuthConnection {
	t.Helper()
	expiresAt := time.Now().UTC().Add(2 * time.Minute)
	encrypted := encryptConnectAuthValues(t, "access-a", "refresh-a")
	conn, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		EndUserRef:            "user_123",
		CreatedByAppID:        f.appID,
		AuthType:              "oauth",
		AuthName:              "oauth",
		EncryptedDEK:          encrypted.dek,
		EncryptedAccessToken:  encrypted.values[0],
		EncryptedRefreshToken: encrypted.values[1],
		TokenType:             "Bearer",
		Scopes:                []string{"openid", "email"},
		Issuer:                "https://issuer.example.com",
		Subject:               "sub-123",
		IdentityClaims:        []byte(`{"email":"user@example.com"}`),
		ExpiresAt:             &expiresAt,
		RefreshState:          "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection bucket A: %v", err)
	}
	return conn
}

func upsertAPIKeyConnection(t *testing.T, f connectAuthFixture) *AuthConnection {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "access-b")
	conn, err := f.store.UpsertAuthConnection(f.ctx, AuthConnection{
		BucketID:             f.bucketB,
		ServiceID:            f.serviceID,
		EndUserRef:           "user_123",
		AuthType:             "api_key",
		AuthName:             "api_key",
		EncryptedDEK:         encrypted.dek,
		EncryptedAccessToken: encrypted.values[0],
		TokenType:            "Bearer",
		RefreshState:         "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection bucket B: %v", err)
	}
	return conn
}

func assertDifferentBucketConnections(t *testing.T, connA, connB *AuthConnection) {
	t.Helper()
	if connA.ID == connB.ID {
		t.Fatal("same end user/service in different buckets must produce separate connections")
	}
}

func assertBucketConnectionLookup(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	assertAuthConnectionByNaturalKey(t, f, connA)
	assertAuthConnectionBucketAccess(t, f, connA)
	assertCrossBucketDeleteBlocked(t, f, connA.ID)
}

func assertAuthConnectionByNaturalKey(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	found, err := f.store.GetAuthConnection(f.ctx, f.bucketA, f.serviceID, "user_123", "oauth")
	if err != nil {
		t.Fatalf("GetAuthConnection: %v", err)
	}
	if found == nil || found.ID != connA.ID || decryptConnectAuthValue(t, found.EncryptedDEK, found.EncryptedAccessToken) != "reconnected-access" {
		t.Fatalf("expected bucket A connection, got %#v", found)
	}
}

func assertAuthConnectionBucketAccess(t *testing.T, f connectAuthFixture, connA *AuthConnection) {
	t.Helper()
	allowed, err := f.store.GetAuthConnectionByIDForBuckets(f.ctx, connA.ID, []uuid.UUID{f.bucketA})
	if err != nil {
		t.Fatalf("GetAuthConnectionByIDForBuckets allowed: %v", err)
	}
	if allowed == nil || allowed.ID != connA.ID {
		t.Fatalf("expected connection through linked bucket, got %#v", allowed)
	}

	blocked, err := f.store.GetAuthConnectionByIDForBuckets(f.ctx, connA.ID, []uuid.UUID{f.bucketB})
	if err != nil {
		t.Fatalf("GetAuthConnectionByIDForBuckets blocked: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected cross-bucket connection lookup to be blocked, got %#v", blocked)
	}
}

func assertCrossBucketDeleteBlocked(t *testing.T, f connectAuthFixture, connID uuid.UUID) {
	t.Helper()
	if err := f.store.DeleteAuthConnection(f.ctx, f.bucketB, connID); !errors.Is(err, ErrAuthConnectionNotFound) {
		t.Fatalf("expected cross-bucket delete to be not found, got %v", err)
	}
}

func assertConnectionListAndRefreshQuery(t *testing.T, f connectAuthFixture, connAID, connBID uuid.UUID) {
	t.Helper()
	connectionsByID, err := f.store.GetAuthConnectionsByIDs(f.ctx, []uuid.UUID{connAID, connBID, connAID})
	if err != nil {
		t.Fatalf("GetAuthConnectionsByIDs: %v", err)
	}
	if len(connectionsByID) != 2 || connectionsByID[connAID].ID != connAID || connectionsByID[connBID].ID != connBID {
		t.Fatalf("batched connections = %#v, want both requested IDs once", connectionsByID)
	}

	serviceFilter := f.serviceID
	listed, err := f.store.ListAuthConnections(f.ctx, f.bucketA, &serviceFilter, "user_123")
	if err != nil {
		t.Fatalf("ListAuthConnections: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != connAID {
		t.Fatalf("expected one bucket-filtered connection, got %#v", listed)
	}

	refreshable, err := f.store.ListAuthConnectionsNeedingRefresh(f.ctx, time.Now().UTC().Add(5*time.Minute), 10)
	if err != nil {
		t.Fatalf("ListAuthConnectionsNeedingRefresh: %v", err)
	}
	if !containsAuthConnection(refreshable, connAID) || containsAuthConnection(refreshable, connBID) {
		t.Fatalf("expected only OAuth connection with refresh token to need refresh, got %#v", refreshable)
	}
}

func assertBucketConnectSummary(t *testing.T, f connectAuthFixture) {
	t.Helper()
	summary, err := f.store.GetBucketConnectSummary(f.ctx, f.bucketA)
	if err != nil {
		t.Fatalf("GetBucketConnectSummary: %v", err)
	}
	if summary.BucketID != f.bucketA || summary.ConnectConfigCount != 1 || summary.ConnectedUserCount != 1 {
		t.Fatalf("unexpected bucket connect summary: %#v", summary)
	}
}

func testConnectSessionLifecycle(t *testing.T, f connectAuthFixture) {
	t.Helper()
	session := createConnectSession(t, f)
	assertConnectSessionLookup(t, f, session)
	markConnectSessionUsed(t, f, session.StateHash)
	deleteExpiredConnectSession(t, f)
}

func createConnectSession(t *testing.T, f connectAuthFixture) *ConnectSession {
	t.Helper()
	encrypted := encryptConnectAuthValues(t, "pkce-verifier")
	session, err := f.store.CreateConnectSession(f.ctx, ConnectSession{
		BucketID:              f.bucketA,
		ServiceID:             f.serviceID,
		AuthType:              "oauth",
		AuthName:              "oauth",
		EndUserRef:            "user_456",
		StateHash:             "state-" + uuid.NewString(),
		NonceHash:             "nonce-hash",
		EncryptedDEK:          encrypted.dek,
		EncryptedPKCEVerifier: encrypted.values[0],
		CreatedByAppID:        f.appID,
		ReturnURL:             "https://app.example.com/oauth/done",
		RequestedScopes:       []string{"test"},
		ExpiresAt:             time.Now().UTC().Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateConnectSession: %v", err)
	}
	return session
}

func assertConnectSessionLookup(t *testing.T, f connectAuthFixture, session *ConnectSession) {
	t.Helper()
	found, err := f.store.GetConnectSessionByStateHash(f.ctx, session.StateHash)
	if err != nil {
		t.Fatalf("GetConnectSessionByStateHash: %v", err)
	}
	if found == nil || found.BucketID != f.bucketA || found.EndUserRef != "user_456" {
		t.Fatalf("unexpected connect session: %#v", found)
	}
	if found.ReturnURL != "https://app.example.com/oauth/done" {
		t.Fatalf("expected return_url to round-trip, got %q", found.ReturnURL)
	}
	if decryptConnectAuthValue(t, found.EncryptedDEK, found.EncryptedPKCEVerifier) != "pkce-verifier" {
		t.Fatal("expected encrypted PKCE verifier to decrypt to fixture value")
	}
}

func markConnectSessionUsed(t *testing.T, f connectAuthFixture, stateHash string) {
	t.Helper()
	if err := f.store.MarkConnectSessionUsed(f.ctx, stateHash, time.Now().UTC()); err != nil {
		t.Fatalf("MarkConnectSessionUsed: %v", err)
	}
	used, err := f.store.GetConnectSessionByStateHash(f.ctx, stateHash)
	if err != nil {
		t.Fatalf("GetConnectSessionByStateHash after mark used: %v", err)
	}
	if used == nil || used.UsedAt == nil {
		t.Fatal("expected connect session to be marked used")
	}
	if err := f.store.MarkConnectSessionUsed(f.ctx, stateHash, time.Now().UTC()); !errors.Is(err, ErrConnectSessionUnavailable) {
		t.Fatalf("expected replayed connect session mark to fail, got %v", err)
	}
}

func deleteExpiredConnectSession(t *testing.T, f connectAuthFixture) {
	t.Helper()
	deleted, err := f.store.DeleteExpiredConnectSessions(f.ctx, time.Now().UTC().Add(30*time.Minute))
	if err != nil {
		t.Fatalf("DeleteExpiredConnectSessions: %v", err)
	}
	if deleted == 0 {
		t.Fatal("expected expired session cleanup to delete at least one row")
	}
}

func containsAuthConnection(connections []AuthConnection, id uuid.UUID) bool {
	for _, conn := range connections {
		if conn.ID == id {
			return true
		}
	}
	return false
}

type encryptedConnectAuthValues struct {
	dek    string
	values []string
}

func encryptConnectAuthValues(t *testing.T, plaintexts ...string) encryptedConnectAuthValues {
	t.Helper()
	wrappedDEK, dek, err := WrapDEK(connectAuthTestMasterKey)
	if err != nil {
		t.Fatalf("wrap connect auth test DEK: %v", err)
	}
	encrypted := make([]string, 0, len(plaintexts))
	for _, plaintext := range plaintexts {
		ciphertext, err := EncryptWithDEK(dek, plaintext)
		if err != nil {
			t.Fatalf("encrypt connect auth test value: %v", err)
		}
		encrypted = append(encrypted, ciphertext)
	}
	return encryptedConnectAuthValues{dek: wrappedDEK, values: encrypted}
}

func decryptConnectAuthValue(t *testing.T, wrappedDEK, ciphertext string) string {
	t.Helper()
	dek, err := UnwrapDEK(connectAuthTestMasterKey, wrappedDEK)
	if err != nil {
		t.Fatalf("unwrap connect auth test DEK: %v", err)
	}
	plaintext, err := DecryptWithDEK(dek, ciphertext)
	if err != nil {
		t.Fatalf("decrypt connect auth test value: %v", err)
	}
	return plaintext
}
