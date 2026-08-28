package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dispatchProfileMasterKey is a fixed 32-byte key so encrypted fixture rows
// (secrets, auth connections) are reproducible across test runs, matching the
// convention used by store package DB-gated tests.
var dispatchProfileMasterKey = []byte("12345678901234567890123456789012")

// TestWorkspaceConnectionProfileDispatch drives the full Batch 3 runtime
// dispatch chain -- scope resolution, real Postgres-backed secret/binding
// resolution, and the HTTP dispatcher -- against a live vendor stub. It
// proves the plan's verification matrix items #7 and #19 plus the Runtime
// Dispatch section's provider-baseline/override/reset/operation-scoping/
// resource-materialization behaviors end to end, not just at the store layer
// (see postgres_store_connect_test.go for the store-only equivalents).
func TestWorkspaceConnectionProfileDispatch(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("Skipping workspace connection profile dispatch test: DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("failed to connect to DB: %v", err)
	}
	defer pool.Close()

	f := setupDispatchProfileFixture(t, ctx, pool)
	defer f.cleanup()

	// Wire the real Postgres store into the same SecretResolver/Dispatcher
	// pair engineExecuteCore uses in production -- no store mocks past this
	// point, so a regression in the SQL binding join or the resolver's
	// resource materialization would fail this test.
	realStore := store.NewPostgresStore(pool)
	resolver := NewSecretResolver(realStore, dispatchProfileMasterKey)
	dispatcher := engine.NewDispatcher()

	origResolver := globalSecretResolver
	globalSecretResolver = resolver
	defer func() { globalSecretResolver = origResolver }()

	var captured capturedVendorRequest
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vendorCallCount++
		captured = capturedVendorRequest{path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone()}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer vendor.Close()
	parsedVendorURL, err := url.Parse(vendor.URL)
	// Derive the loopback host from the actual listener so the explicit routing allowlist remains portable across IP families.
	if err != nil {
		t.Fatalf("parse vendor URL: %v", err)
	}

	f.seedConnectedResource(t, vendor.URL)

	obj := &fusedobject.ServiceMetadata{
		ID:          f.serviceID,
		Name:        "DispatchProfileSvc",
		BaseURL:     vendor.URL,
		AuthConfigs: fusedobject.AuthConfigs{{Name: "bearerAuth", Type: "oauth2"}},
		ConnectConfig: &fusedobject.ServiceConnectConfig{
			ResourceInput: &fusedobject.ResourceInputConfig{AllowedHosts: []string{parsedVendorURL.Hostname()}},
		},
	}
	selections := []models.SDKSelection{{
		ServiceID: f.serviceID, ServiceVersionID: f.versionID,
		SchemaVersion: models.AppSelectionSchemaVersion, EndpointIDs: []uuid.UUID{f.epID},
		AuthType: "oauth", AuthName: "bearerAuth",
	}}
	scopeJSON, err := json.Marshal(selections)
	if err != nil {
		t.Fatalf("marshal scope selections: %v", err)
	}
	// The operation must declare the exact named scheme because service-wide auth definitions alone cannot authorize credential use.
	securityRequirements := singleAuthRequirement("bearerAuth")
	pathlessCache := &richMockCache{scopeJSON: scopeJSON, obj: obj, epID: f.epID, securityRequirements: securityRequirements}
	pathedCache := &richMockCache{
		scopeJSON: scopeJSON, obj: obj, epID: f.epID, path: "/items/{accountId}", method: http.MethodGet,
		securityRequirements: securityRequirements,
		parameters: fusedobject.Parameters{{
			Name: "accountId", In: "path", Required: true,
			Schema: &fusedobject.SchemaContract{Projection: fusedobject.Schema{Type: "string"}},
		}},
	}

	h := &dispatchProfileHarness{
		ctx: ctx, dispatcher: dispatcher, f: f, captured: &captured,
		pathlessCache: pathlessCache, pathedCache: pathedCache,
		creds: map[string]any{"fused_end_user_ref": f.endUserRef},
	}

	f.seedBaseline(t)

	// Each concern below is its own top-level function (not an inline t.Run
	// closure) so cyclomatic complexity is measured and stays bounded per
	// concern, matching this repo's complexity ceiling of 10 per function --
	// see assertBaselineFallbackDispatch and friends.
	t.Run("provider baseline fallback dispatch", func(t *testing.T) { assertBaselineFallbackDispatch(t, h) })
	t.Run("workspace override changes dispatch immediately", func(t *testing.T) { assertOverrideChangesDispatchImmediately(t, h) })
	t.Run("operation scoped bindings only apply to matching operations", func(t *testing.T) { assertOperationScopedBindings(t, h) })
	t.Run("resource derived materialization for base_url header query and path", func(t *testing.T) { assertResourceDerivedMaterialization(t, h) })
	t.Run("reset reverts dispatch to baseline", func(t *testing.T) { assertResetRevertsToBaseline(t, h) })
	t.Run("rejects a service outside the artifact scope even though the bucket has matching material", func(t *testing.T) { assertOutOfScopeServiceRejected(t, h) })
}

// dispatchProfileHarness bundles the shared dispatch dependencies (fixture,
// dispatcher, vendor-request capture, and the two scope-cache shapes) each
// subtest function below needs, so extracting subtests into named top-level
// functions doesn't require a long parameter list per function.
type dispatchProfileHarness struct {
	ctx                        context.Context
	dispatcher                 *engine.Dispatcher
	f                          *dispatchProfileFixture
	captured                   *capturedVendorRequest
	pathlessCache, pathedCache *richMockCache
	creds                      map[string]any
}

// dispatch runs one engineExecuteCore call through the real resolver/dispatcher
// pair, resetting the shared captured-request snapshot first so assertions
// only ever see this call's outbound request.
func (h *dispatchProfileHarness) dispatch(t *testing.T, cache *richMockCache, endpointName string) error {
	t.Helper()
	*h.captured = capturedVendorRequest{}
	return engineExecuteCore(h.ctx, cache, h.dispatcher, &dummyTokenValidator{}, h.f.appID.String(), "tok",
		endpointName, map[string]any{}, cloneCredMap(h.creds), "", engine.NewBufferStream())
}

// assertBaselineFallbackDispatch proves dispatch resolves the pinned baseline
// binding when no workspace override exists (Runtime Dispatch step 5, plan
// verification item 7's "no override" case).
func assertBaselineFallbackDispatch(t *testing.T, h *dispatchProfileHarness) {
	if err := h.dispatch(t, h.pathlessCache, "list_items"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := h.captured.header.Get("X-Source"); got != "baseline" {
		t.Fatalf("X-Source header = %q, want %q (baseline binding not applied)", got, "baseline")
	}
}

// assertOverrideChangesDispatchImmediately proves an override upsert changes
// the next dispatch's bindings with no restart and no explicit cache eviction
// call, confirming binding reads stay live (see cached_store.go's pass-through
// wiring for workspace profile methods).
func assertOverrideChangesDispatchImmediately(t *testing.T, h *dispatchProfileHarness) {
	h.f.upsertOverride(t, []store.WorkspaceConnectionBinding{literalHeaderBinding("X-Source", "override", nil)})
	if err := h.dispatch(t, h.pathlessCache, "list_items"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := h.captured.header.Get("X-Source"); got != "override" {
		t.Fatalf("X-Source header = %q, want %q -- override should win over baseline without any cache eviction call", got, "override")
	}
}

// assertOperationScopedBindings proves a binding scoped to one operation ID
// only applies to dispatches for that exact operation, while an unscoped
// binding on the same profile still applies everywhere.
func assertOperationScopedBindings(t *testing.T, h *dispatchProfileHarness) {
	h.f.upsertOverride(t, []store.WorkspaceConnectionBinding{
		literalHeaderBinding("X-Source", "override", nil),
		literalHeaderBinding("X-Op-Scoped", "yes", []string{"do_thing"}),
	})

	if err := h.dispatch(t, h.pathlessCache, "list_items"); err != nil {
		t.Fatalf("dispatch list_items: %v", err)
	}
	if got := h.captured.header.Get("X-Op-Scoped"); got != "" {
		t.Fatalf("list_items unexpectedly received operation-scoped header %q", got)
	}

	if err := h.dispatch(t, h.pathlessCache, "do_thing"); err != nil {
		t.Fatalf("dispatch do_thing: %v", err)
	}
	if got := h.captured.header.Get("X-Op-Scoped"); got != "yes" {
		t.Fatalf("do_thing did not receive its operation-scoped header, got %q", got)
	}
	if got := h.captured.header.Get("X-Source"); got != "override" {
		t.Fatalf("do_thing lost the all-operations override header, got %q", got)
	}
}

// assertResourceDerivedMaterialization proves ${resource.*} bindings for
// every target location the plan calls out (base_url, header, query, path)
// are materialized from a real bucket-scoped connection resource and applied
// by the dispatcher in the same request.
func assertResourceDerivedMaterialization(t *testing.T, h *dispatchProfileHarness) {
	resourcePath := "base_url"
	providerIDPath := "provider_resource_id"
	regionPath := "metadata.region"
	accountPath := "metadata.account_id"
	h.f.upsertOverride(t, []store.WorkspaceConnectionBinding{
		{SourceKind: "connection_resource", SourcePath: &resourcePath, TargetLocation: "base_url", Mode: "force", Provenance: "workspace"},
		{SourceKind: "connection_resource", SourcePath: &providerIDPath, TargetLocation: "header", TargetName: "X-Provider-Resource", Mode: "force", Provenance: "workspace"},
		{SourceKind: "connection_resource", SourcePath: &regionPath, TargetLocation: "query", TargetName: "region", Mode: "force", Provenance: "workspace"},
		{SourceKind: "connection_resource", SourcePath: &accountPath, TargetLocation: "path", TargetName: "accountId", Mode: "force", Provenance: "workspace"},
	})

	if err := h.dispatch(t, h.pathedCache, "list_items"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if h.captured.path != "/tenant-a/items/acct-42" {
		t.Fatalf("materialized path = %q, want forced base_url prefix + path substitution", h.captured.path)
	}
	if h.captured.rawQuery != "region=eu" {
		t.Fatalf("materialized query = %q, want %q", h.captured.rawQuery, "region=eu")
	}
	if got := h.captured.header.Get("X-Provider-Resource"); got != "res-1" {
		t.Fatalf("materialized provider_resource_id header = %q, want %q", got, "res-1")
	}
	// Sanity check the connected-auth token still made it through the same
	// dispatch that also resolved dynamic resource bindings.
	if got := h.captured.header.Get("Authorization"); got != "Bearer connected-access-token" {
		t.Fatalf("connected auth token missing/altered by resource materialization: %q", got)
	}
}

// assertResetRevertsToBaseline proves deleting the override reverts the very
// next dispatch to the pinned baseline binding set, with no leftover override
// or resource-derived headers from the prior subtest.
func assertResetRevertsToBaseline(t *testing.T, h *dispatchProfileHarness) {
	profileStore := h.f.profileStore(t)
	if err := profileStore.ResetWorkspaceProfile(h.ctx, h.f.serviceID, h.f.versionID, "oauth"); err != nil {
		t.Fatalf("ResetWorkspaceProfile: %v", err)
	}
	if err := h.dispatch(t, h.pathlessCache, "list_items"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := h.captured.header.Get("X-Source"); got != "baseline" {
		t.Fatalf("X-Source header after reset = %q, want %q", got, "baseline")
	}
	if got := h.captured.header.Get("X-Op-Scoped"); got != "" {
		t.Fatalf("reset should have removed the override's operation-scoped binding, got %q", got)
	}
	if got := h.captured.header.Get("X-Provider-Resource"); got != "" {
		t.Fatalf("reset should have removed the override's resource bindings, got %q", got)
	}
}

// assertOutOfScopeServiceRejected is the plan's verification item 19: a
// dispatch for a service/version outside the artifact's own scope must be
// rejected even though the same bucket-reachable workspace holds a real,
// retrievable baseline binding for that other service.
func assertOutOfScopeServiceRejected(t *testing.T, h *dispatchProfileHarness) {
	// Prove the "matching material" premise: Service B's own baseline binding
	// really is retrievable for this bucket via the same execution-scoped
	// query dispatch uses -- it is not simply absent from the database.
	profileStore := h.f.profileStore(t)
	leaked, err := profileStore.ListWorkspaceBindingsForExecution(h.ctx, h.f.bucketID, h.f.outOfScopeServiceID, h.f.outOfScopeVersionID, "oauth", "list_items")
	if err != nil || len(leaked) != 1 || leaked[0].TargetName != "X-Leaked-Service-B" {
		t.Fatalf("expected service B's binding to be genuinely present in this bucket's workspace, got %#v, err=%v", leaked, err)
	}

	// The SDK's own persisted scope selects only service A (see
	// setupDispatchProfileFixture), so an endpoint name that scope doesn't
	// expose must be rejected before any vendor call or credential lookup --
	// service B's binding must never reach this dispatch regardless of it
	// sitting in the same bucket-reachable workspace.
	callCountBefore := vendorCallCount
	if err := h.dispatch(t, h.pathlessCache, "nonexistent_tool"); err == nil {
		t.Fatal("expected out-of-scope endpoint dispatch to fail")
	}
	if callCountBefore != vendorCallCount {
		t.Fatalf("out-of-scope dispatch must never reach the vendor")
	}

	// A normal in-scope dispatch for service A must never carry service B's
	// marker header, proving no cross-service leakage through the bucket join.
	if err := h.dispatch(t, h.pathlessCache, "list_items"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := h.captured.header.Get("X-Leaked-Service-B"); got != "" {
		t.Fatalf("service A dispatch must never receive service B's binding, got %q", got)
	}
}

// capturedVendorRequest snapshots exactly what the stub vendor server
// received so assertions read the actual outbound HTTP request rather than
// intermediate resolver state.
type capturedVendorRequest struct {
	path     string
	rawQuery string
	header   http.Header
}

// vendorCallCount is incremented indirectly through captured -- tests read it
// as a proxy for "did the vendor handler run" via the outer closure captured
// variable's reset-then-check pattern; kept as a package-level counter only
// for the explicit not-dispatched assertion in the scope-rejection subtest.
var vendorCallCount int

func cloneCredMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// literalHeaderBinding builds a forced literal header binding, optionally
// scoped to a fixed operation set -- shared by the override/operation-scoping
// subtests so binding construction doesn't drift between them.
func literalHeaderBinding(name, value string, operationIDs []string) store.WorkspaceConnectionBinding {
	return store.WorkspaceConnectionBinding{
		SourceKind: "literal", LiteralValue: &value, TargetLocation: "header", TargetName: name,
		Mode: "force", Provenance: "workspace", OperationIDs: operationIDs,
	}
}

// dispatchProfileFixture owns the DB rows this test seeds directly against
// Postgres (bucket, sdk scope, service versions, connected auth) so the
// runtime dispatch chain resolves real rows instead of mocks.
type dispatchProfileFixture struct {
	t                   *testing.T
	ctx                 context.Context
	pool                *pgxpool.Pool
	store               store.Store
	workspaceID         uuid.UUID
	accountID           uuid.UUID
	ownsWorkspace       bool
	bucketID            uuid.UUID
	serviceID           uuid.UUID
	versionID           uuid.UUID
	outOfScopeServiceID uuid.UUID
	outOfScopeVersionID uuid.UUID
	appID               uuid.UUID
	appFamilyID         uuid.UUID
	ownerTeamID         uuid.UUID
	epID                uuid.UUID
	endUserRef          string
	connectionID        uuid.UUID
}

// setupDispatchProfileFixture seeds the current Engine identities required to exercise real PostgreSQL dispatch resolution.
func setupDispatchProfileFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *dispatchProfileFixture {
	t.Helper()
	workspaceID, accountID, ownsWorkspace := dispatchProfileWorkspace(t, ctx, pool)
	f := &dispatchProfileFixture{
		t: t, ctx: ctx, pool: pool, store: store.NewPostgresStore(pool),
		workspaceID: workspaceID, accountID: accountID, ownsWorkspace: ownsWorkspace,
		bucketID: uuid.New(), serviceID: uuid.New(), versionID: uuid.New(),
		outOfScopeServiceID: uuid.New(), outOfScopeVersionID: uuid.New(),
		appID: uuid.New(), appFamilyID: uuid.New(), ownerTeamID: uuid.New(), epID: uuid.New(), endUserRef: "dispatch-profile-user",
	}
	if _, err := pool.Exec(ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, $2, $3)`,
		f.ownerTeamID, "Dispatch profile owner", "dispatch-profile-owner-"+f.ownerTeamID.String()); err != nil {
		t.Fatalf("seed active artifact owner team: %v", err)
	}

	// fused_buckets dropped its workspace_id column during the mono-workspace
	// migration (Engine has exactly one workspace, so bucket identity no
	// longer needs it) -- f.workspaceID still exists for fused_workspaces
	// itself and as the account owner reference below, just not here.
	if _, err := pool.Exec(ctx, `INSERT INTO fused_buckets (id, name) VALUES ($1, $2)`,
		f.bucketID, "dispatch-profile-"+f.bucketID.String()); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	if err := f.store.AddWorkspaceServiceVersion(ctx, f.serviceID, "", "v1", f.versionID, "Dispatch Profile Service A", f.accountID); err != nil {
		t.Fatalf("activate service A version: %v", err)
	}
	if err := f.store.AddWorkspaceServiceVersion(ctx, f.outOfScopeServiceID, "", "v1", f.outOfScopeVersionID, "Dispatch Profile Service B", f.accountID); err != nil {
		t.Fatalf("activate service B version: %v", err)
	}

	// The persisted scope selects only service A -- this is the artifact's
	// durable scope the "rejects out-of-scope service" subtest depends on.
	selections := []models.SDKSelection{{
		ServiceID: f.serviceID, ServiceVersionID: f.versionID,
		SchemaVersion: models.AppSelectionSchemaVersion, EndpointIDs: []uuid.UUID{f.epID},
		AuthType: "oauth", AuthName: "bearerAuth",
	}}
	selectionsJSON, err := json.Marshal(selections)
	if err != nil {
		t.Fatalf("marshal sdk scope selections: %v", err)
	}
	// Keep the related runtime rows atomic while issuing one command per prepared statement, as production PostgreSQL requires.
	if err := f.seedAppRuntime(selectionsJSON); err != nil {
		t.Fatalf("seed SDK app runtime: %v", err)
	}

	encryptedToken := dispatchEncrypt(t, "connected-access-token")
	conn, err := f.store.UpsertAuthConnection(ctx, store.AuthConnection{
		BucketID: f.bucketID, ServiceID: f.serviceID, ServiceVersionID: f.versionID,
		EndUserRef: f.endUserRef, CreatedByAppID: f.appID, AuthType: "oauth", AuthName: "bearerAuth",
		EncryptedDEK:         encryptedToken.dek,
		EncryptedAccessToken: encryptedToken.values[0],
		TokenType:            "Bearer", RefreshState: "ok",
	})
	if err != nil {
		t.Fatalf("UpsertAuthConnection: %v", err)
	}
	f.connectionID = conn.ID

	// Service B's own baseline + binding live in the same bucket-reachable
	// workspace as service A -- the "matching material" the scope-rejection
	// subtest proves never leaks into a service A dispatch.
	leakedName := "X-Leaked-Service-B"
	leakedValue := "leaked"
	batchStore := f.profileStore(t)
	registryProfileID := uuid.New()
	if err := batchStore.ReconcileWorkspaceProfiles(ctx, []store.WorkspaceProfileReplacement{{
		Profile: store.WorkspaceConnectionProfile{
			ServiceID: f.outOfScopeServiceID, ServiceVersionID: f.outOfScopeVersionID,
			AuthType: "oauth", Layer: "baseline", RegistryProfileID: &registryProfileID,
			ProfileRevision: 1, ProfileHash: "service-b-hash", Provenance: "provider",
			ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
		},
		Bindings: []store.WorkspaceConnectionBinding{{
			ServiceID: f.outOfScopeServiceID, ServiceVersionID: f.outOfScopeVersionID,
			SourceKind: "literal", LiteralValue: &leakedValue, TargetLocation: "header", TargetName: leakedName,
			Mode: "force", Provenance: "provider",
		}},
	}}, nil); err != nil {
		t.Fatalf("seed service B baseline: %v", err)
	}

	return f
}

// seedAppRuntime creates one internally consistent SDK version fixture without relying on multi-command prepared statements.
func (f *dispatchProfileFixture) seedAppRuntime(selectionsJSON []byte) error {
	return pgx.BeginFunc(f.ctx, f.pool, func(tx pgx.Tx) error {
		// The family must exist before its immutable version can reference it.
		if _, err := tx.Exec(f.ctx, `
			INSERT INTO fused_app_families
				(app_family_id, account_id, kind, canonical_name, display_name, target_language, owner_team_id)
			VALUES ($1, $2, 'sdk', $3, 'Dispatch profile SDK', 'typescript', $4)
		`, f.appFamilyID, f.accountID, "dispatch-profile-"+f.appFamilyID.String(), f.ownerTeamID); err != nil {
			return fmt.Errorf("insert app family: %w", err)
		}
		// Persist the exact immutable version and selection scope exercised by dispatch.
		if _, err := tx.Exec(f.ctx, `
			INSERT INTO fused_apps
				(app_id, app_family_id, account_id, version, config_key, source_hash, status, scope_schema_version, selections)
			VALUES ($1, $2, $3, '1.0.0', $4, 'dispatch-profile', 'active', $5, $6)
		`, f.appID, f.appFamilyID, f.accountID, "sdk:dispatch-profile:"+f.appFamilyID.String(), models.AppScopeSchemaVersion, selectionsJSON); err != nil {
			return fmt.Errorf("insert app version: %w", err)
		}
		// Bind the family only after both lifecycle identities exist, preserving the same FK-safe order as production writes.
		if _, err := tx.Exec(f.ctx, `
			INSERT INTO fused_app_family_buckets (app_family_id, bucket_id) VALUES ($1, $2)
		`, f.appFamilyID, f.bucketID); err != nil {
			return fmt.Errorf("bind app family bucket: %w", err)
		}
		return nil
	})
}

// seedConnectedResource attaches the one connection resource used by the
// resource-materialization subtest. baseURL is the vendor stub's own URL plus
// a tenant path segment so the test can assert the forced base_url binding
// actually redirected the request, not just that it didn't error.
func (f *dispatchProfileFixture) seedConnectedResource(t *testing.T, vendorURL string) {
	t.Helper()
	_, err := f.store.ReconcileConnectionResources(f.ctx, f.connectionID, []store.ConnectionResource{{
		ConnectionID: f.connectionID, BucketID: f.bucketID, ServiceID: f.serviceID,
		ProviderResourceID: "res-1", ResourceType: "tenant", DisplayName: "Tenant A",
		BaseURL: vendorURL + "/tenant-a", MetadataJSON: []byte(`{"region":"eu","account_id":"acct-42"}`),
	}})
	if err != nil {
		t.Fatalf("ReconcileConnectionResources: %v", err)
	}
}

// seedBaseline attaches service A's pinned baseline (mirrors what activation
// does independently of any bucket) with one all-operations header binding,
// giving the "provider baseline fallback" and "reset" subtests something
// stable to fall back to.
func (f *dispatchProfileFixture) seedBaseline(t *testing.T) store.WorkspaceConnectionProfile {
	t.Helper()
	registryProfileID := uuid.New()
	batchStore := f.profileStore(t)
	baselineBinding := literalHeaderBinding("X-Source", "baseline", nil)
	baselineBinding.ServiceID, baselineBinding.ServiceVersionID = f.serviceID, f.versionID
	if err := batchStore.ReconcileWorkspaceProfiles(f.ctx, []store.WorkspaceProfileReplacement{{
		Profile: store.WorkspaceConnectionProfile{
			ServiceID: f.serviceID, ServiceVersionID: f.versionID,
			AuthType: "oauth", Layer: "baseline", RegistryProfileID: &registryProfileID,
			ProfileRevision: 1, ProfileHash: "service-a-baseline-hash", Provenance: "provider",
			ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
		},
		Bindings: []store.WorkspaceConnectionBinding{baselineBinding},
	}}, nil); err != nil {
		t.Fatalf("seed service A baseline: %v", err)
	}
	profileStore := f.profileStore(t)
	stored, err := profileStore.GetEffectiveWorkspaceProfile(f.ctx, f.serviceID, f.versionID, "oauth")
	if err != nil || stored == nil {
		t.Fatalf("load seeded baseline: %#v, err=%v", stored, err)
	}
	return *stored
}

// upsertOverride replaces the workspace override's full binding set --
// UpsertWorkspaceProfileOverride is always a full replace, matching the
// plan's "editing an effective profile performs an upsert" rule.
func (f *dispatchProfileFixture) upsertOverride(t *testing.T, bindings []store.WorkspaceConnectionBinding) {
	t.Helper()
	for i := range bindings {
		bindings[i].ServiceID = f.serviceID
		bindings[i].ServiceVersionID = f.versionID
	}
	profileStore := f.profileStore(t)
	_, err := profileStore.UpsertWorkspaceProfileOverride(f.ctx, store.WorkspaceConnectionProfile{
		ServiceID: f.serviceID, ServiceVersionID: f.versionID,
		AuthType: "oauth", ProfileRevision: 1, ProfileHash: "service-a-override-hash",
		ProfileSnapshot: []byte(`{"auth_type":"oauth"}`),
	}, bindings)
	if err != nil {
		t.Fatalf("UpsertWorkspaceProfileOverride: %v", err)
	}
}

// profileStore combines WorkspaceProfileStore and WorkspaceProfileBatchStore
// through one narrow local interface so callers get both capabilities from a
// single type assertion instead of repeating it at every call site.
type dispatchProfileStore interface {
	store.WorkspaceProfileStore
	store.WorkspaceProfileBatchStore
}

func (f *dispatchProfileFixture) profileStore(t *testing.T) dispatchProfileStore {
	t.Helper()
	ps, ok := f.store.(dispatchProfileStore)
	if !ok {
		t.Fatal("store does not implement the workspace profile store capabilities")
	}
	return ps
}

func (f *dispatchProfileFixture) cleanup() {
	ctx := context.Background()
	if f.ownsWorkspace {
		_, _ = f.pool.Exec(ctx, `DELETE FROM fused_workspaces WHERE id = $1`, f.workspaceID)
	}
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_apps WHERE app_id = $1`, f.appID)
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_app_families WHERE app_family_id = $1`, f.appFamilyID)
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_buckets WHERE id = $1`, f.bucketID)
	// Neither table below carries workspace_id anymore post mono-workspace
	// migration -- service_id alone already scopes these rows uniquely.
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_workspace_connection_profiles WHERE service_id IN ($1, $2)`,
		f.serviceID, f.outOfScopeServiceID)
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_workspace_services WHERE service_id IN ($1, $2)`,
		f.serviceID, f.outOfScopeServiceID)
	_, _ = f.pool.Exec(ctx, `DELETE FROM fused_teams WHERE id = $1`, f.ownerTeamID)
}

// dispatchProfileWorkspace reuses the Engine singleton workspace when present
// (mirrors store.connectAuthWorkspace) because creating a second workspace
// would violate the production schema's singleton constraint.
func dispatchProfileWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID, bool) {
	t.Helper()
	var workspaceID, accountID uuid.UUID
	err := pool.QueryRow(ctx, `SELECT id, account_id FROM fused_workspaces LIMIT 1`).Scan(&workspaceID, &accountID)
	if err == nil {
		return workspaceID, accountID, false
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("load dispatch profile workspace: %v", err)
	}
	workspaceID, accountID = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO fused_workspaces (id, account_id, name, slug) VALUES ($1, $2, $3, $4)`,
		workspaceID, accountID, "Dispatch Profile Workspace", "dispatch-profile-"+uuid.NewString()); err != nil {
		t.Fatalf("seed dispatch profile workspace: %v", err)
	}
	return workspaceID, accountID, true
}

type dispatchEncryptedValue struct {
	dek    string
	values []string
}

// dispatchEncrypt wraps a DEK and encrypts one plaintext with it so seeded
// rows exercise the resolver's real decrypt path instead of opaque strings.
func dispatchEncrypt(t *testing.T, plaintext string) dispatchEncryptedValue {
	t.Helper()
	wrappedDEK, dek, err := store.WrapDEK(dispatchProfileMasterKey)
	if err != nil {
		t.Fatalf("wrap dispatch profile DEK: %v", err)
	}
	ciphertext, err := store.EncryptWithDEK(dek, plaintext)
	if err != nil {
		t.Fatalf("encrypt dispatch profile value: %v", err)
	}
	return dispatchEncryptedValue{dek: wrappedDEK, values: []string{ciphertext}}
}
