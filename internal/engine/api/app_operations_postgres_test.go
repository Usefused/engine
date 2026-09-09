package api

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mcpOperationPostgresFixture owns isolated identities for one persisted exact-version catalogue.
type mcpOperationPostgresFixture struct {
	ctx              context.Context
	pool             *pgxpool.Pool
	repository       store.Store
	snapshotStore    store.ServiceContractSnapshotStore
	catalogueStore   store.AppCatalogRepository
	accountID        uuid.UUID
	familyID         uuid.UUID
	appID            uuid.UUID
	teamID           uuid.UUID
	planID           uuid.UUID
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	listEndpointID   uuid.UUID
	createEndpointID uuid.UUID
}

// TestLoadMCPAppOperationCatalogueFromPostgres verifies select-all expansion and Unified discovery use persisted exact-version authority.
func TestLoadMCPAppOperationCatalogueFromPostgres(t *testing.T) {
	fixture := openMCPAppOperationPostgresFixture(t)
	fixture.seedTeamAndSnapshot(t)
	fixture.seedMCPVersion(t)
	fixture.assertCatalogue(t)
}

// openMCPAppOperationPostgresFixture initializes production storage and registers exact-row cleanup before returning.
func openMCPAppOperationPostgresFixture(t *testing.T) mcpOperationPostgresFixture {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	// PostgreSQL integration is opt-in so ordinary package tests never guess a developer database.
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	// Context cleanup is registered first so fixture deletion and pool shutdown retain a live deadline.
	t.Cleanup(cancel)
	pool, err := db.InitEnginePostgres(ctx, databaseURL)
	// Schema initialization must succeed before this test writes isolated fixture identities.
	if err != nil {
		t.Fatalf("initialize Engine PostgreSQL: %v", err)
	}
	// Pool cleanup is registered before exact-row cleanup so the later registration executes first.
	t.Cleanup(pool.Close)

	repository := store.NewPostgresStore(pool)
	snapshotStore, ok := repository.(store.ServiceContractSnapshotStore)
	// The production PostgreSQL store must expose the exact local service-contract snapshot boundary.
	if !ok {
		t.Fatal("PostgreSQL store does not support service contract snapshots")
	}
	catalogueStore, ok := repository.(store.AppCatalogRepository)
	// The API read must use the same authorization-aware app catalogue implemented by production storage.
	if !ok {
		t.Fatal("PostgreSQL store does not support app catalogue reads")
	}
	fixture := mcpOperationPostgresFixture{
		ctx: ctx, pool: pool, repository: repository, snapshotStore: snapshotStore, catalogueStore: catalogueStore,
		accountID: uuid.New(), familyID: uuid.New(), appID: uuid.New(), teamID: uuid.New(), planID: uuid.New(),
		serviceID: uuid.New(), serviceVersionID: uuid.New(), listEndpointID: uuid.New(), createEndpointID: uuid.New(),
	}
	// Exact-row cleanup is the last registration and therefore runs before the pool closes.
	t.Cleanup(func() { fixture.cleanup(t) })
	return fixture
}

// cleanup removes only rows owned by this fixture while the registered PostgreSQL pool remains open.
func (fixture mcpOperationPostgresFixture) cleanup(t *testing.T) {
	t.Helper()
	deletes := []struct {
		query string
		id    uuid.UUID
	}{
		{`DELETE FROM fused_config_plans WHERE id=$1`, fixture.planID},
		{`DELETE FROM fused_apps WHERE app_id=$1`, fixture.appID},
		{`DELETE FROM fused_app_families WHERE app_family_id=$1`, fixture.familyID},
		{`DELETE FROM fused_service_contract_snapshots WHERE service_version_id=$1`, fixture.serviceVersionID},
		{`DELETE FROM fused_teams WHERE id=$1`, fixture.teamID},
	}
	// Ordered deletes honor foreign keys without broadening cleanup beyond generated UUIDs.
	for _, deletion := range deletes {
		_, err := fixture.pool.Exec(context.Background(), deletion.query, deletion.id)
		// Cleanup failures are test failures because leaked integration rows would make later runs misleading.
		if err != nil {
			t.Errorf("cleanup PostgreSQL MCP operation fixture: %v", err)
		}
	}
}

// seedTeamAndSnapshot persists the local contract rows required to expand one select-all service.
func (fixture mcpOperationPostgresFixture) seedTeamAndSnapshot(t *testing.T) {
	t.Helper()
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_teams (id, name, slug) VALUES ($1, $2, $3)`, fixture.teamID, "MCP operation test owners", "mcp-operation-test-"+fixture.teamID.String())
	// App-family ownership requires a real local team row even though the catalogue read uses account-wide authorization.
	if err != nil {
		t.Fatalf("seed owner team: %v", err)
	}
	snapshot := store.ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		ServiceID:                 fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Version: "2026-09-09",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, Name: "Tickets", BaseURL: "https://tickets.example.test"},
		Endpoints: []fusedobject.Endpoint{
			{ID: fixture.listEndpointID, Name: "tickets.list", Method: "GET", Path: "/tickets", NormalizedPath: "/tickets"},
			{ID: fixture.createEndpointID, Name: "tickets.create", Method: "POST", Path: "/tickets", NormalizedPath: "/tickets"},
		},
	}
	_, err = fixture.snapshotStore.UpsertServiceContractSnapshot(fixture.ctx, snapshot)
	// Select-all can expand only after the immutable Engine-local endpoint rows exist.
	if err != nil {
		t.Fatalf("seed service contract snapshot: %v", err)
	}
}

// seedMCPVersion publishes immutable selection state and its integrity-pinned applied Unified descriptor.
func (fixture mcpOperationPostgresFixture) seedMCPVersion(t *testing.T) {
	t.Helper()
	family, _, err := fixture.repository.CreateOrGetAppFamily(fixture.ctx, store.AppFamily{
		AppFamilyID: fixture.familyID, AccountID: fixture.accountID, Kind: store.AppKindMCP,
		CanonicalName: "support-" + fixture.familyID.String(), DisplayName: "Support MCP", OwnerTeamID: fixture.teamID,
	})
	// The version must belong to an MCP family so SDK identities cannot enter this catalogue.
	if err != nil {
		t.Fatalf("seed MCP family: %v", err)
	}
	descriptors := &models.SDKUnifiedOperationDescriptors{SchemaVersion: models.SDKUnifiedDescriptorSchemaVersion, Operations: []models.SDKUnifiedOperationDescriptor{{
		Name: "support.resolve", InputSchema: json.RawMessage(`{"type":"object"}`), Targets: []models.SDKUnifiedTargetDescriptor{{
			PublicTarget: "tickets", OperationID: "tickets.list", ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, EndpointID: fixture.listEndpointID,
		}},
	}}}
	descriptorJSON, err := json.Marshal(descriptors)
	// The stored plan and immutable app hash must be derived from the exact same descriptor bytes.
	if err != nil {
		t.Fatalf("encode Unified descriptor: %v", err)
	}
	descriptorDigest, err := canonicaljson.HexSHA256(descriptorJSON)
	// Canonical hashing is the integrity boundary used by the production descriptor read.
	if err != nil {
		t.Fatalf("hash Unified descriptor: %v", err)
	}
	selectionsJSON, err := json.Marshal([]models.SDKSelection{{
		SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: fixture.serviceID, ServiceVersionID: fixture.serviceVersionID, SelectAll: true,
	}})
	// The immutable app row must retain select-all intent instead of materializing a mutable endpoint list.
	if err != nil {
		t.Fatalf("encode MCP selections: %v", err)
	}
	sourceHash := "sha256:" + strings.Repeat("b", 64)
	configKey := "mcp:Support MCP:" + fixture.appID.String()
	_, _, err = fixture.repository.PublishAppVersion(fixture.ctx, store.App{
		AppID: fixture.appID, AppFamilyID: family.AppFamilyID, AccountID: fixture.accountID, Version: "2.0.0",
		ConfigKey: configKey, SourceHash: sourceHash, CapabilityHash: "mcp-operation-test",
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selectionsJSON,
		UnifiedDefinitionSchemaVersion: store.UnifiedDefinitionSchemaVersion,
		UnifiedDefinitions:             []byte("[]"), UnifiedDefinitionHash: store.EmptyUnifiedSetHash,
		UnifiedCodegenDescriptorHash: "sha256:" + descriptorDigest,
		Status:                       store.AppStatusActive, ExpectedFamilyKind: store.AppKindMCP,
	})
	// Publication freezes the exact service selection and expected public Unified descriptor hash.
	if err != nil {
		t.Fatalf("publish MCP version: %v", err)
	}
	resolvedPayload, err := json.Marshal(map[string]any{"unified_operations": descriptors})
	// The applied plan remains the sole recoverable public descriptor source for this exact version.
	if err != nil {
		t.Fatalf("encode applied MCP plan: %v", err)
	}
	_, err = fixture.pool.Exec(fixture.ctx, `
		INSERT INTO fused_config_plans
			(id, config_key, config_type, owner_team_id, source_hash, status,
			 actions, desired_state, resolved_payload, blockers, warnings,
			 required_permissions, applied_at)
		VALUES ($1,$2,'mcp',$3,$4,'applied','[]','{}',$5,'[]','[]','[]',NOW())
	`, fixture.planID, configKey, fixture.teamID, sourceHash, resolvedPayload)
	// Descriptor recovery requires the exact applied config-key and source-hash pair pinned by the app row.
	if err != nil {
		t.Fatalf("seed applied MCP plan: %v", err)
	}
}

// assertCatalogue reads through production PostgreSQL projections and verifies the deterministic complete allowlist.
func (fixture mcpOperationPostgresFixture) assertCatalogue(t *testing.T) {
	t.Helper()
	catalogue := fixture.loadCatalogue(t)
	assertMCPAppOperationCatalogueIdentity(t, fixture, catalogue)
	assertMCPAppOperationCatalogueEntries(t, catalogue)
}

// loadCatalogue composes the exact authorized app with its physical and Unified PostgreSQL projections.
func (fixture mcpOperationPostgresFixture) loadCatalogue(t *testing.T) mcpAppOperationCatalogue {
	t.Helper()
	item, err := fixture.catalogueStore.GetAuthorizedApp(fixture.ctx, fixture.accountID, fixture.appID, accesscontrol.AuthorizedScope{All: true})
	// The catalogue input must come through the same exact-app authorization query used by GraphQL.
	if err != nil {
		t.Fatalf("read authorized MCP version: %v", err)
	}
	catalogue, err := loadMCPAppOperationCatalogue(fixture.ctx, fixture.repository, *item)
	// A successful read proves both PostgreSQL projections can compose without Registry fallback or per-row queries.
	if err != nil {
		t.Fatalf("load MCP operation catalogue: %v", err)
	}
	return catalogue
}

// assertMCPAppOperationCatalogueIdentity verifies the merged result remains bound to the seeded immutable MCP version.
func assertMCPAppOperationCatalogueIdentity(t *testing.T, fixture mcpOperationPostgresFixture, catalogue mcpAppOperationCatalogue) {
	t.Helper()
	// Version IDs and complete row count must survive both independent PostgreSQL projections.
	if catalogue.AppID != fixture.appID || catalogue.AppFamilyID != fixture.familyID || len(catalogue.Operations) != 3 {
		t.Fatalf("unexpected MCP operation catalogue: %#v", catalogue)
	}
}

// assertMCPAppOperationCatalogueEntries verifies select-all expansion and Unified names merge in deterministic public order.
func assertMCPAppOperationCatalogueEntries(t *testing.T, catalogue mcpAppOperationCatalogue) {
	t.Helper()
	// Sorting and kind labels make the command output deterministic while preserving complete select-all expansion.
	wantIDs := []string{"support.resolve", "tickets.create", "tickets.list"}
	for index, wantID := range wantIDs {
		// Every expected invocation name must occupy its stable lexicographic position.
		if catalogue.Operations[index].OperationID != wantID {
			t.Fatalf("operation[%d] = %#v, want %q", index, catalogue.Operations[index], wantID)
		}
	}
	// Unified identity and physical provenance remain distinguishable after merging.
	if catalogue.Operations[0].Kind != appOperationKindUnified || catalogue.Operations[1].Kind != appOperationKindPhysical || catalogue.Operations[2].Kind != appOperationKindPhysical {
		t.Fatalf("unexpected MCP operation kinds: %#v", catalogue.Operations)
	}
}
